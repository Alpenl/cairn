package service

import (
	"net/http"
	"strings"

	"webtag/internal/dto"
	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/service/urlmeta"
)

// This file owns the dto.IngestRequest → normalizedIngest pipeline:
// the per-kind handlers (URL / text / image / browser_capture), the
// accumulator that collects per-source state, the source-key /
// synthetic-URL derivation, and the data:image/* size guard. Wave 12
// L-4 split it out of ingest.go so the service file stays focused on
// the IngestService struct + the public Ingest method.

type normalizedIngest struct {
	sourceKind           string
	sourceKey            string
	storedURL            string
	realURL              string
	lockURL              string
	description          string
	inputTitle           string
	inputText            string
	inputHTML            string
	inputImages          []string
	sourceMetadata       map[string]any
	requestedLibraryKind model.RequestedLibraryKind
	destination          string
	predictedLibraryKind *model.LibraryKind
}

type ingestSourceRecord struct {
	Kind      string         `json:"kind"`
	URL       string         `json:"url,omitempty"`
	Title     string         `json:"title,omitempty"`
	Text      string         `json:"text,omitempty"`
	HTML      string         `json:"html,omitempty"`
	ImageURLs []string       `json:"image_urls,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

func (n normalizedIngest) toLinkCapture() (LinkCapture, error) {
	// Replaces a previous reflect-driven setStructFields helper. Direct
	// assignments are simpler to read, faster, and surface field-name typos
	// at compile time instead of at runtime.
	intent := resolveRequestedLibraryIntent(requestedLibraryIntent{}, userRequestedLibraryIntent(n.requestedLibraryKind))
	params := LinkCapture{
		URL:                        n.storedURL,
		Destination:                n.destination,
		Status:                     model.LinkStatusPending,
		SourceKind:                 n.sourceKind,
		SourceKey:                  n.sourceKey,
		InputImages:                n.inputImages,
		SourceMetadata:             n.sourceMetadata,
		RequestedLibraryKind:       intent.Kind,
		RequestedLibraryKindSource: intent.Source,
		PredictedLibraryKind:       n.predictedLibraryKind,
	}
	if description := strings.TrimSpace(n.description); description != "" {
		copied := description
		params.Description = &copied
	}
	if title := strings.TrimSpace(n.inputTitle); title != "" {
		copied := title
		params.InputTitle = &copied
	}
	if text := strings.TrimSpace(n.inputText); text != "" {
		copied := text
		params.InputText = &copied
	}
	if html := strings.TrimSpace(n.inputHTML); html != "" {
		copied := html
		params.InputHTML = &copied
	}
	return params, nil
}

// ingestAccumulator collects the per-source state during ingest
// normalization. Splitting it out from normalizeIngestRequest lets each
// source-kind handler mutate the accumulator instead of every kind
// closing over a long set of locals; the outer loop becomes a thin
// dispatch.
type ingestAccumulator struct {
	records     []ingestSourceRecord
	imagesSeen  map[string]struct{}
	inputImages []string
	metadata    map[string]any
	textParts   []string
	htmlParts   []string
	firstTitle  string
	realURL     string
	captureURL  string
	// captureSubmittedURL tracks the spelling paired with captureURL. A
	// browser capture owns URL identity even when a generic URL source appears
	// first, so its display value must win as well.
	captureSubmittedURL string
	// submittedURL is the first URL as the client actually wrote it, before
	// normalisation. It is evidence, never identity: nothing downstream may
	// look anything up with it.
	submittedURL string
	userNote     string
}

func newIngestAccumulator(capacity int) *ingestAccumulator {
	return &ingestAccumulator{
		records:     make([]ingestSourceRecord, 0, capacity),
		imagesSeen:  map[string]struct{}{},
		inputImages: make([]string, 0),
		metadata:    map[string]any{},
	}
}

// addImageOnce dedupes appended image URLs so the same image referenced
// across multiple sources lands in inputImages exactly once.
func (a *ingestAccumulator) addImageOnce(imageURL string) {
	if _, seen := a.imagesSeen[imageURL]; seen {
		return
	}
	a.imagesSeen[imageURL] = struct{}{}
	a.inputImages = append(a.inputImages, imageURL)
}

// noteRealURL captures the first non-empty URL encountered so the
// stored link's URL field tracks an actual remote resource when one is
// present (browser_capture or url-kind sources).
func (a *ingestAccumulator) noteRealURL(rawURL string) {
	if a.realURL == "" {
		a.realURL = rawURL
	}
}

// noteSubmittedURL records the pre-normalisation spelling of the first URL
// source so the stored record can still show where it came from.
func (a *ingestAccumulator) noteSubmittedURL(submitted string) {
	if a.submittedURL == "" {
		a.submittedURL = strings.TrimSpace(submitted)
	}
}

// noteCaptureURL records the page the browser actually captured. A generic
// url source may appear before the browser_capture record, but it is
// supplemental input rather than the identity of the saved bookmark.
func (a *ingestAccumulator) noteCaptureURL(rawURL, submittedURL string) {
	a.captureURL = rawURL
	a.captureSubmittedURL = strings.TrimSpace(submittedURL)
	a.noteRealURL(rawURL)
}

// normalizeIngestRequest 把多源 IngestRequest 折叠成单条入库所需的统一结构：
// 逐源派发到对应 handler 累积内容、根据来源组合推断 sourceKind、用稳定哈希
// 算出 sourceKey 作为去重键，并在缺少真实 URL 时合成 webtag://ingest/* 占位。
func normalizeIngestRequest(req dto.IngestRequest, defaultDestination string) (normalizedIngest, error) { //nolint:gocyclo // 多模态入口：url/text/image/capture 各有独立归一化路径
	// Wave 9 MED 迁移：/api/ingest 入口的三种 422 都补上 slug，
	// 帮上游"用户拿到错误后想自动重试还是抛错给人类"区分场景。
	if len(req.Sources) == 0 {
		return normalizedIngest{}, httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeIngestSourceRequired, "at least one source is required")
	}
	requestedKind, err := normalizeRequestedLibraryKind(req.RequestedLibraryKind)
	if err != nil {
		return normalizedIngest{}, err
	}
	destination, requestedKind, err := normalizeIngestCaptureTarget(req.Destination, requestedKind, defaultDestination)
	if err != nil {
		return normalizedIngest{}, err
	}

	acc, err := accumulateIngestSources(req.Sources)
	if err != nil {
		return normalizedIngest{}, err
	}

	sourceKind := determineSourceKind(acc.records)
	identityURL := acc.realURL
	if acc.captureURL != "" {
		identityURL = acc.captureURL
	}
	var sourceKey string
	switch {
	case acc.captureURL != "":
		// A browser capture is a new snapshot of one bookmark, not a new
		// bookmark identity. Volatile metadata such as captured_at must not
		// defeat database-level deduplication across application replicas.
		sourceKey = acc.captureURL
	case identityURL != "":
		// Any URL-backed ingest is an alternate transport for saving that page,
		// even when text or images accompany it. Keep the content on the capture,
		// but use the same identity as Submit/Batch so another entry point cannot
		// create a second link or parse job for the page.
		sourceKey = identityURL
	default:
		sourceKey, err = buildSourceKey(acc.records)
		if err != nil {
			return normalizedIngest{}, err
		}
	}

	displayURL := acc.submittedURL
	if acc.captureURL != "" {
		displayURL = acc.captureSubmittedURL
	}
	submittedDisplayURL := displayURL
	classificationURL := identityURL
	if displayURL == "" {
		displayURL = syntheticIngestURL(sourceKey)
		classificationURL = displayURL
	}
	if acc.captureURL != "" {
		fingerprint, err := buildCaptureSourceFingerprint(acc.records)
		if err != nil {
			return normalizedIngest{}, err
		}
		if fingerprint != "" {
			acc.metadata[captureSourceFingerprintMetadataKey] = fingerprint
		}
	}

	metadata := withOriginalURL(acc.metadata, submittedDisplayURL, identityURL)
	if len(metadata) == 0 {
		metadata = nil
	}
	decision := ClassifyLibrary(ClassificationInput{
		RequestedKind:  requestedKind,
		SourceKind:     sourceKind,
		URL:            classificationURL,
		URLMetadata:    urlmeta.ClassifyURL(classificationURL),
		CaptureSignals: captureSignalsFromMetadata(metadata),
	})
	var predictedLibraryKind *model.LibraryKind
	if requestedKind == model.RequestedLibraryKindAuto && decision.Kind != "" {
		predicted := decision.Kind
		predictedLibraryKind = &predicted
	}

	return normalizedIngest{
		sourceKind:           sourceKind,
		sourceKey:            sourceKey,
		storedURL:            displayURL,
		realURL:              identityURL,
		lockURL:              identityURL,
		description:          acc.userNote,
		inputTitle:           acc.firstTitle,
		inputText:            strings.Join(acc.textParts, "\n\n"),
		inputHTML:            strings.Join(acc.htmlParts, "\n\n"),
		inputImages:          acc.inputImages,
		sourceMetadata:       metadata,
		requestedLibraryKind: requestedKind,
		destination:          destination,
		predictedLibraryKind: predictedLibraryKind,
	}, nil
}

// normalizeIngestCaptureTarget keeps the public site destination explicit
// while reusing the existing site-library intent all the way through the
// collection pipeline. A contradictory reading intent is rejected rather
// than silently changing either caller choice.
func normalizeIngestCaptureTarget(
	rawDestination string,
	requestedKind model.RequestedLibraryKind,
	defaultDestination string,
) (string, model.RequestedLibraryKind, error) {
	if strings.EqualFold(strings.TrimSpace(rawDestination), captureDestinationSite) {
		if requestedKind != model.RequestedLibraryKindAuto && requestedKind != model.RequestedLibraryKindSite {
			return "", "", httperr.NewWithCode(
				http.StatusUnprocessableEntity,
				httperr.CodeInvalidRequestedLibraryKind,
				"site destination requires requested_library_kind to be auto or site",
			)
		}
		return captureDestinationSite, model.RequestedLibraryKindSite, nil
	}
	destination, err := normalizeCaptureDestination(rawDestination, defaultDestination)
	return destination, requestedKind, err
}

func accumulateIngestSources(sources []dto.IngestSource) (*ingestAccumulator, error) {
	acc := newIngestAccumulator(len(sources))
	for _, src := range sources {
		kind := strings.TrimSpace(strings.ToLower(src.Kind))
		if kind == "" {
			return nil, httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeIngestSourceKindRequired, "source kind is required")
		}

		record := ingestSourceRecord{Kind: kind}
		var err error
		switch kind {
		case "url":
			err = handleURLSource(acc, src, &record)
		case "text":
			err = handleTextSource(acc, src, &record)
		case "image":
			err = handleImageSource(acc, src, &record)
		case "browser_capture":
			err = handleBrowserCaptureSource(acc, src, &record)
		default:
			return nil, httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeUnsupportedIngestSourceKind, "unsupported source kind")
		}
		if err != nil {
			return nil, err
		}

		acc.records = append(acc.records, record)
	}
	return acc, nil
}

// captureSignalsFromMetadata is deliberately an allow-list projection. Capture
// metadata can include user-visible descriptions and other bounded values, but
// classifier inputs must never gain a path to body text or arbitrary JSON.
func captureSignalsFromMetadata(metadata map[string]any) CaptureSignals {
	// Browser-capture metadata is persisted below its source namespace so it
	// cannot collide with other source metadata. Keep accepting a direct map
	// for this small mapper's focused unit tests, but production ingest only
	// promotes the browser_capture namespace.
	if browserCapture, ok := metadata["browser_capture"].(map[string]any); ok {
		metadata = browserCapture
	}
	value := func(key string) string {
		text, _ := metadata[key].(string)
		return strings.TrimSpace(text)
	}
	flag := func(key string) bool { return strings.EqualFold(value(key), "true") }
	var types []string
	for _, raw := range strings.Split(value("jsonld_types"), ",") {
		if item := strings.TrimSpace(raw); item != "" && len(item) <= 64 && len(types) < 20 {
			types = append(types, item)
		}
	}
	return CaptureSignals{
		OGType:                value("og_type"),
		JSONLDTypes:           types,
		HasApplicationName:    flag("has_application_name"),
		HasWebAppManifest:     flag("has_web_app_manifest"),
		HasAuthorAndPublished: flag("has_author_and_published"),
		ProseDominant:         flag("prose_dominant"),
		InteractiveDominant:   flag("interactive_dominant"),
		NavigationDominant:    flag("navigation_dominant"),
		SiteDescription:       flag("has_site_description"),
	}
}

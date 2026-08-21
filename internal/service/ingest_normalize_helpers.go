package service

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	"webtag/internal/jsonx"
	"webtag/internal/problem"
)

const captureSourceFingerprintMetadataKey = "capture_source_fingerprint"

type captureSourceIdentity struct {
	Kind string `json:"kind"`
	URL  string `json:"url"`
}

func determineSourceKind(records []ingestSourceRecord) string {
	if len(records) == 1 {
		return records[0].Kind
	}
	return "multimodal"
}

func buildSourceKey(records []ingestSourceRecord) (string, error) {
	payload, err := jsonx.Marshal(records)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return "ingest:" + hex.EncodeToString(sum[:]), nil
}

// buildCaptureSourceFingerprint returns an order-independent fingerprint for
// identity-bearing sources attached to a browser capture. The captured page
// itself is deliberately excluded because SourceKey already represents that
// bookmark identity. Text and metadata are also excluded: their meaningful
// content is compared separately, and volatile metadata such as captured_at
// must never turn an otherwise identical save into new work.
func buildCaptureSourceFingerprint(records []ingestSourceRecord) (string, error) {
	seen := make(map[captureSourceIdentity]struct{}, len(records))
	for _, record := range records {
		if record.Kind == "browser_capture" || strings.TrimSpace(record.URL) == "" {
			continue
		}
		seen[captureSourceIdentity{Kind: record.Kind, URL: record.URL}] = struct{}{}
	}
	if len(seen) == 0 {
		return "", nil
	}

	identities := make([]captureSourceIdentity, 0, len(seen))
	for identity := range seen {
		identities = append(identities, identity)
	}
	sort.Slice(identities, func(i, j int) bool {
		if identities[i].Kind == identities[j].Kind {
			return identities[i].URL < identities[j].URL
		}
		return identities[i].Kind < identities[j].Kind
	})
	payload, err := jsonx.Marshal(identities)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func syntheticIngestURL(sourceKey string) string {
	hash := strings.TrimPrefix(sourceKey, "ingest:")
	if len(hash) > 12 {
		hash = hash[:12]
	}
	return "webtag://ingest/" + hash
}

// maxImageDataURLBytes caps any single data:image/* URL accepted by
// ingest. The /api/ingest endpoint already limits the JSON body
// (handler.go 里的 max-body cap 那种做法), but a
// caller could otherwise spend the whole 4 MiB request budget on one
// inline base64 image, leaving no room for the rest of the payload
// and bloating the persisted input_images JSONB column. 1 MiB is
// generous for embedded screenshots while keeping the worst case
// bounded.
const maxImageDataURLBytes = 1 << 20

// validateImageLocator 校验图片来源：支持远程 http/https URL 或 data:image/*
// base64 URL；后者额外做体积上限保护，防止单张图把整请求预算占满。
func validateImageLocator(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		// 空字符串与"非 http(s)/data URL"共用同一个 slug：客户端拿到稳定 token
		// 即可分支处理，不需要二次解析 message。
		return "", problem.NewWithCode(problem.Invalid, problem.CodeIngestImageSourceRequired, "image source requires a remote http/https URL or data URL")
	}
	if strings.HasPrefix(strings.ToLower(raw), "data:image/") {
		if len(raw) > maxImageDataURLBytes {
			return "", problem.NewWithCode(problem.Invalid, problem.CodeIngestImageDataURLTooLarge, "image data url exceeds size limit")
		}
		return raw, nil
	}
	validatedURL, err := validateURL(raw)
	if err != nil {
		return "", problem.NewWithCode(problem.Invalid, problem.CodeIngestImageSourceRequired, "image source requires a remote http/https URL or data URL")
	}
	return validatedURL, nil
}

// Bounds for ingest source metadata. The 4 MiB request body cap is the
// only structural guard upstream, so without these a single key or value
// could swallow most of the budget and bloat the persisted JSONB column.
// The numbers are conservative defaults — large enough that real
// browser-capture metadata (a handful of typed fields, short string
// values) fits trivially, small enough that abusive payloads are
// rejected before allocation.
const (
	maxIngestMetadataKeys        = 64
	maxIngestMetadataKeyLength   = 128
	maxIngestMetadataValueLength = 4096
)

// validateIngestMetadata enforces the bounds above on a top-level
// metadata map. Nested values are not recursed into: JSON depth is
// already bounded by the request body cap, and the column accepts
// arbitrary nested JSON by design. Errors include the field name and
// the limit but never echo the offending key/value back to the caller.
func validateIngestMetadata(meta map[string]any, fieldName string) error {
	if len(meta) > maxIngestMetadataKeys {
		return problem.NewWithCode(problem.Invalid, problem.CodeIngestMetadataKeyCountExceeded,
			fieldName+" exceeds key count limit")
	}
	for key, value := range meta {
		if len(key) > maxIngestMetadataKeyLength {
			return problem.NewWithCode(problem.Invalid, problem.CodeIngestMetadataKeyLengthExceeded,
				fieldName+" key length exceeds limit")
		}
		if s, ok := value.(string); ok && len(s) > maxIngestMetadataValueLength {
			return problem.NewWithCode(problem.Invalid, problem.CodeIngestMetadataValueLengthExceeded,
				fieldName+" string value length exceeds limit")
		}
	}
	return nil
}

// cloneMap returns a shallow copy of src — top-level keys are duplicated
// but nested map / slice values are shared by reference with the input.
// This is intentional: the contract from this point forward is that
// accumulated metadata is read-only.
//
//   - The repository serializes the map through json.Marshal before
//     storing as JSONB, so the persisted blob is independent of any
//     post-call mutation.
//   - No code path after handoff to LinkCapture /
//     UpdateAnalysisParams mutates a value in place.
//   - validateIngestMetadata caps the depth/width before the clone,
//     so a deep-clone here would just double the allocation cost
//     without changing observable behavior.
//
// The previous incarnation (cloneAndSortMap) iterated over a sorted
// key slice on the assumption it would influence iteration order
// downstream — but Go's map iteration order is intentionally
// randomized, so the sort had no observable effect on the returned
// map. The deterministic ordering buildSourceKey relies on comes from
// json.Marshal, which sorts keys itself when serializing map[string]T.
func cloneMap(src map[string]any) map[string]any {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]any, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

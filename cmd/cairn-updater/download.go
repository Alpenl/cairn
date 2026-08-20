package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"webtag/internal/releasetrust"
)

// The metadata ceilings bound both small signed documents and the larger pages
// returned by GitHub's release collection. A response without a ceiling could
// exhaust the helper's memory before any signature has been checked.
const (
	maxMetadataBytes    int64 = 1 << 20
	maxDiscoveryBytes   int64 = 8 << 20
	releasesPerPage           = 100
	maxReleasePages           = 100
	githubAPIVersion          = "2026-03-10"
	githubJSONMediaType       = "application/vnd.github+json"
)

// Downloader fetches release assets under the download policy in
// internal/releasetrust.
//
// The helper never takes a URL from anywhere. Every request in this file is
// built by releasetrust.AssetURL from the compiled-in repository and a tag that
// has already been matched against the formal vX.Y.Z pattern, and the client's
// redirect policy re-checks every hop against the host allow-list. GitHub does
// redirect asset downloads to an object store, so "follow no redirects" is not
// an option — but "follow whatever Location says" would turn a spoofed release
// endpoint into an arbitrary fetch running as root.
type Downloader struct {
	client *http.Client
	repo   string
}

// NewDownloader builds a downloader with the release download policy installed.
func NewDownloader(repo string) *Downloader {
	return &Downloader{
		client: &http.Client{
			CheckRedirect: releasetrust.CheckDownloadRedirect,
			Timeout:       DownloadTimeout,
		},
		repo: repo,
	}
}

// Asset downloads one release asset of an exact tag.
//
// limit is a hard ceiling on the bytes read. For archives the caller passes the
// size the signed manifest declares, so a response that grew past the signed
// size is refused while it is still streaming rather than after it has already
// been buffered.
func (downloader *Downloader) Asset(ctx context.Context, tag, assetName string, limit int64) ([]byte, error) {
	target, err := releasetrust.AssetURL(downloader.repo, tag, assetName)
	if err != nil {
		return nil, err
	}
	return downloader.get(ctx, target.String(), limit)
}

func (downloader *Downloader) get(ctx context.Context, rawURL string, limit int64) ([]byte, error) {
	return downloader.getWithMediaType(ctx, rawURL, limit, "application/octet-stream", "")
}

func (downloader *Downloader) getJSON(ctx context.Context, rawURL string, limit int64) ([]byte, error) {
	return downloader.getWithMediaType(ctx, rawURL, limit, githubJSONMediaType, githubAPIVersion)
}

func (downloader *Downloader) getWithMediaType(
	ctx context.Context,
	rawURL string,
	limit int64,
	mediaType string,
	apiVersion string,
) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request for %s: %w", rawURL, err)
	}
	// Checking the first URL here as well as every redirect hop closes the gap
	// where a caller reached get directly with a URL AssetURL did not build.
	if err := releasetrust.CheckDownloadURL(request.URL); err != nil {
		return nil, err
	}
	request.Header.Set("Accept", mediaType)
	if apiVersion != "" {
		request.Header.Set("X-GitHub-Api-Version", apiVersion)
	}
	request.Header.Set("User-Agent", "cairn-updater/"+fmt.Sprint(releasetrust.HelperProtocol))

	response, err := downloader.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", rawURL, err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: server answered %s", rawURL, response.Status)
	}
	// Read one byte past the ceiling so an oversized body is detectable rather
	// than silently truncated into something that would then fail a hash check
	// with a misleading message.
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", rawURL, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("download %s exceeds its %d byte ceiling", rawURL, limit)
	}
	return data, nil
}

// SignedRelease is a manifest and its detached signature, exactly as received.
//
// The bytes are kept verbatim and passed to verification unchanged. Re-encoding
// them and verifying the result would verify a document nobody sent.
type SignedRelease struct {
	ManifestBytes  []byte
	SignatureBytes []byte
}

// FetchSignedManifest downloads the manifest and signature for an exact tag.
func (downloader *Downloader) FetchSignedManifest(ctx context.Context, tag string) (SignedRelease, error) {
	manifest, err := downloader.Asset(ctx, tag, releasetrust.ManifestFileName, maxMetadataBytes)
	if err != nil {
		return SignedRelease{}, err
	}
	signature, err := downloader.Asset(ctx, tag, releasetrust.SignatureFileName, maxMetadataBytes)
	if err != nil {
		return SignedRelease{}, err
	}
	return SignedRelease{ManifestBytes: manifest, SignatureBytes: signature}, nil
}

// LatestTag finds the highest formal Core patch in the current Core series.
//
// The repository contains releases for several components, and GitHub's
// repository-level "latest" marker can legitimately point at Android or the
// extension. Discovery therefore enumerates releases, ignores component tags,
// drafts and prereleases, and stays in the major/minor series derived from the
// running Core. A minor or major transition must be performed explicitly.
func (downloader *Downloader) LatestTag(ctx context.Context, currentTag string) (string, error) {
	current, err := parseReleaseTag(currentTag)
	if err != nil {
		return "", fmt.Errorf("select release series: %w", err)
	}

	type release struct {
		TagName    string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}

	var latest *releaseVersion
	for page := 1; page <= maxReleasePages; page++ {
		endpoint := &url.URL{
			Scheme: "https",
			Host:   releasetrust.ReleaseAPIHost,
			Path:   "/repos/" + downloader.repo + "/releases",
		}
		query := endpoint.Query()
		query.Set("per_page", strconv.Itoa(releasesPerPage))
		query.Set("page", strconv.Itoa(page))
		endpoint.RawQuery = query.Encode()

		data, err := downloader.getJSON(ctx, endpoint.String(), maxDiscoveryBytes)
		if err != nil {
			return "", err
		}
		var payload []release
		if err := json.Unmarshal(data, &payload); err != nil {
			return "", fmt.Errorf("parse release discovery page %d: %w", page, err)
		}
		for _, candidate := range payload {
			if candidate.Draft || candidate.Prerelease {
				continue
			}
			version, err := parseReleaseTag(candidate.TagName)
			if err != nil || !version.sameSeries(current) {
				continue
			}
			if latest == nil || version.patch > latest.patch {
				copy := version
				latest = &copy
			}
		}
		if len(payload) < releasesPerPage {
			break
		}
		if page == maxReleasePages {
			return "", fmt.Errorf("release discovery exceeded %d pages", maxReleasePages)
		}
	}
	if latest == nil {
		return "", fmt.Errorf("no formal Core release was found in the v%d.%d patch series", current.major, current.minor)
	}
	return latest.tag, nil
}

// discoveryTimeout bounds the read-only latest-release lookup. It is short: an
// unreachable GitHub must degrade the "check for updates" panel, not hang it.
const discoveryTimeout = 20 * time.Second

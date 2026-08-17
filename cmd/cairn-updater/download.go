package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"webtag/internal/releasetrust"
)

// maxMetadataBytes bounds the manifest, the signature, and the discovery
// response. They are small documents; a multi-gigabyte "manifest" is either a
// mistake or an attempt to exhaust the helper's memory before any signature has
// been checked.
const maxMetadataBytes int64 = 1 << 20

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
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request for %s: %w", rawURL, err)
	}
	// Checking the first URL here as well as every redirect hop closes the gap
	// where a caller reached get directly with a URL AssetURL did not build.
	if err := releasetrust.CheckDownloadURL(request.URL); err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/octet-stream")
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

// LatestTag asks GitHub which release is newest.
//
// This is discovery and nothing more. The answer is a suggestion the operator
// has to confirm as an exact tag; it never becomes an install target on its
// own. That is why the result is validated against the formal tag pattern here
// rather than trusted downstream: a repository that answered "main" or
// "v2.0.0-rc1" gets rejected at the boundary, not somewhere deeper where the
// value has already been used to build a path.
func (downloader *Downloader) LatestTag(ctx context.Context) (string, error) {
	rawURL := "https://" + releasetrust.ReleaseAPIHost + "/repos/" + downloader.repo + "/releases/latest"
	data, err := downloader.get(ctx, rawURL, maxMetadataBytes)
	if err != nil {
		return "", err
	}
	var payload struct {
		TagName    string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return "", fmt.Errorf("parse release discovery response: %w", err)
	}
	if payload.Draft || payload.Prerelease {
		return "", errors.New("the newest release is a draft or prerelease and is not an install target")
	}
	if !IsFormalTag(payload.TagName) {
		return "", fmt.Errorf("release discovery returned %q, which is not a formal vX.Y.Z tag", payload.TagName)
	}
	return payload.TagName, nil
}

// discoveryTimeout bounds the read-only latest-release lookup. It is short: an
// unreachable GitHub must degrade the "check for updates" panel, not hang it.
const discoveryTimeout = 20 * time.Second

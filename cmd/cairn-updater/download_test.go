package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"webtag/internal/releasetrust"
)

// The download policy is the boundary that keeps "install release X" from
// becoming "fetch and execute whatever this URL says, as root". These tests
// drive the real policy: real AssetURL construction, the real allow-list, the
// real redirect hook.

func TestAssetURLsAreBuiltNotAccepted(t *testing.T) {
	target, err := releasetrust.AssetURL(Repository, "v1.2.3", releasetrust.ManifestFileName)
	if err != nil {
		t.Fatalf("build asset URL: %v", err)
	}
	want := "https://github.com/Alpenl/cairn/releases/download/v1.2.3/cairn-release-manifest.json"
	if target.String() != want {
		t.Fatalf("expected %s, got %s", want, target)
	}

	// Nothing that is not a plain asset name of a formal tag may become a URL.
	for _, bad := range []struct{ tag, asset string }{
		{"latest", "x.tar.gz"},
		{"main", "x.tar.gz"},
		{"v1.2", "x.tar.gz"},
		{"v1.2.3-rc1", "x.tar.gz"},
		{"v1.2.3", "../../../etc/passwd"},
		{"v1.2.3", "a/b.tar.gz"},
		{"v1.2.3", "a?b"},
		{"v1.2.3", "a#b"},
		{"v1.2.3", ""},
	} {
		if _, err := releasetrust.AssetURL(Repository, bad.tag, bad.asset); err == nil {
			t.Fatalf("tag %q asset %q was turned into a URL", bad.tag, bad.asset)
		}
	}
}

func TestARedirectOffTheAllowListAbortsTheDownload(t *testing.T) {
	// A real off-domain host that would happily serve a payload.
	offDomain := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte("payload from somewhere else entirely"))
	}))
	defer offDomain.Close()

	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, offDomain.URL, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	previous, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"https://github.com/Alpenl/cairn/releases/download/v1.2.3/x.tar.gz", nil)
	if err != nil {
		t.Fatalf("build previous request: %v", err)
	}
	if err := releasetrust.CheckDownloadRedirect(request, []*http.Request{previous}); err == nil {
		t.Fatal("a redirect to an off-allow-list host was permitted")
	}

	// The object store GitHub actually redirects to is permitted, because
	// forbidding redirects outright would break every real download.
	permitted, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"https://objects.githubusercontent.com/whatever", nil)
	if err != nil {
		t.Fatalf("build permitted request: %v", err)
	}
	if err := releasetrust.CheckDownloadRedirect(permitted, []*http.Request{previous}); err != nil {
		t.Fatalf("the release object store must stay reachable: %v", err)
	}
}

func TestLookalikeHostsAreNotOnTheAllowList(t *testing.T) {
	for _, raw := range []string{
		"https://evil-githubusercontent.com/x",
		"https://github.com.attacker.net/x",
		"https://objects.githubusercontent.com.attacker.net/x",
		"https://notgithub.com/x",
		"http://github.com/x",
		"https://user:pass@github.com/x",
	} {
		parsed, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse %s: %v", raw, err)
		}
		if err := releasetrust.CheckDownloadURL(parsed); err == nil {
			t.Fatalf("%s was accepted by the download policy", raw)
		}
	}
}

func TestADownloadThatGrowsPastItsSignedSizeIsRefused(t *testing.T) {
	host := newHost(t)
	// The signed manifest declares an exact size. A response that keeps going
	// must be cut off while it streams rather than buffered and then rejected.
	oversized := make([]byte, host.fixture.Manifest.Core[0].SizeBytes+4096)
	copy(oversized, host.fixture.CoreArchive)
	host.assets.assets[host.fixture.Manifest.Core[0].Archive] = oversized

	job := host.runUpdate(fixtureTag)
	assertHold(t, job, PhaseDownload, HoldEnvironment)
	if !strings.Contains(job.Hold.Detail, "ceiling") {
		t.Fatalf("expected a size ceiling failure, got %q", job.Hold.Detail)
	}
	if host.serviceWasStopped() {
		t.Fatal("an oversized download must never reach the service")
	}
}

func TestAMissingAssetIsAnEnvironmentHoldNotATrustHold(t *testing.T) {
	host := newHost(t)
	// A release whose assets are missing is a broken publish, not evidence of
	// tampering. Classifying it as a supply-chain event would train operators
	// to ignore the class that matters.
	host.assets.failAsset = host.fixture.Manifest.Reader.Archive

	job := host.runUpdate(fixtureTag)
	assertHold(t, job, PhaseDownload, HoldEnvironment)
	if host.serviceWasStopped() {
		t.Fatal("a failed download must never reach the service")
	}
}

func TestTheDownloaderRefusesAURLItDidNotBuild(t *testing.T) {
	downloader := NewDownloader(Repository)
	if _, err := downloader.get(context.Background(), "https://evil.example/payload.tar.gz", 1024); err == nil {
		t.Fatal("the downloader fetched a URL outside the allow-list")
	}
	if _, err := downloader.get(context.Background(), "file:///etc/passwd", 1024); err == nil {
		t.Fatal("the downloader accepted a file URL")
	}
}

func TestTheAllowListIsExactAndCopied(t *testing.T) {
	hosts := releasetrust.AllowedDownloadHosts()
	if len(hosts) == 0 {
		t.Fatal("the allow-list is empty")
	}
	hosts[0] = "attacker.example"
	if releasetrust.AllowedDownloadHosts()[0] == "attacker.example" {
		t.Fatal("the allow-list is mutable by its callers")
	}
}

func TestLatestTagUsesTheJSONAPIAndStaysInTheCurrentPatchSeries(t *testing.T) {
	requests := 0
	api := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		if request.URL.Path != "/repos/Alpenl/cairn/releases" {
			t.Errorf("discovery path = %q, want the release collection", request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		if request.Header.Get("Accept") != "application/vnd.github+json" {
			writer.WriteHeader(http.StatusUnsupportedMediaType)
			return
		}
		if request.Header.Get("X-GitHub-Api-Version") != githubAPIVersion {
			t.Errorf("GitHub API version = %q, want %q", request.Header.Get("X-GitHub-Api-Version"), githubAPIVersion)
		}
		if request.URL.Query().Get("per_page") != strconv.Itoa(releasesPerPage) {
			t.Errorf("per_page = %q, want %d", request.URL.Query().Get("per_page"), releasesPerPage)
		}

		page, err := strconv.Atoi(request.URL.Query().Get("page"))
		if err != nil {
			t.Errorf("invalid page query: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		releases := make([]map[string]any, 0, releasesPerPage)
		switch page {
		case 1:
			for index := range releasesPerPage {
				releases = append(releases, map[string]any{
					"tag_name":   "android/v0.1." + strconv.Itoa(index),
					"draft":      false,
					"prerelease": false,
				})
			}
			releases[0] = map[string]any{"tag_name": "v0.1.16", "draft": false, "prerelease": false}
			releases[1] = map[string]any{"tag_name": "v0.2.0", "draft": false, "prerelease": false}
			releases[2] = map[string]any{"tag_name": "v0.1.99", "draft": true, "prerelease": false}
			releases[3] = map[string]any{"tag_name": "v0.1.98", "draft": false, "prerelease": true}
		case 2:
			releases = append(releases,
				map[string]any{"tag_name": "extension/v0.9.0", "draft": false, "prerelease": false},
				map[string]any{"tag_name": "v0.1.17", "draft": false, "prerelease": false},
			)
		default:
			t.Errorf("unexpected discovery page %d", page)
		}
		writer.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(writer).Encode(releases); err != nil {
			t.Errorf("encode releases: %v", err)
		}
	}))
	defer api.Close()

	base, err := url.Parse(api.URL)
	if err != nil {
		t.Fatalf("parse test API URL: %v", err)
	}
	downloader := NewDownloader(Repository)
	downloader.client.Transport = &rewriteTransport{host: base.Host}

	tag, err := downloader.LatestTag(context.Background(), "v0.1.15")
	if err != nil {
		t.Fatalf("discover latest tag: %v", err)
	}
	if tag != "v0.1.17" {
		t.Fatalf("latest tag = %q, want v0.1.17", tag)
	}
	if requests != 2 {
		t.Fatalf("discovery requests = %d, want 2 pages", requests)
	}
}

func TestLatestTagRejectsAnInvalidCurrentReleaseBeforeNetworkAccess(t *testing.T) {
	downloader := NewDownloader(Repository)
	downloader.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("an invalid current release must fail before network access")
		return nil, nil
	})

	for _, current := range []string{"0.1.15", "v0.1", "v0.1.15-rc1", "v00.1.15", "v0.0.0"} {
		if _, err := downloader.LatestTag(context.Background(), current); err == nil {
			t.Fatalf("current release %q was accepted", current)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

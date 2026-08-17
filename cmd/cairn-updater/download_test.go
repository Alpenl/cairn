package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
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

package fetcher

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// writeYtdlpStub creates an executable shell script that writes the given
// stdout, stderr and exits with the given status. Used in place of a real
// yt-dlp binary to exercise YtdlpFetcher.
func writeYtdlpStub(t *testing.T, stdout, stderr string, exitCode int) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "yt-dlp-stub.sh")
	script := "#!/bin/sh\n"
	if stderr != "" {
		script += "cat >&2 <<'EOF_STUB_STDERR'\n" + stderr + "\nEOF_STUB_STDERR\n"
	}
	if stdout != "" {
		script += "cat <<'EOF_STUB_STDOUT'\n" + stdout + "\nEOF_STUB_STDOUT\n"
	}
	if exitCode != 0 {
		script += "exit " + strconv.Itoa(exitCode) + "\n"
	}
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return path
}

const ytdlpHappyJSON = `{
	"title": "Rick Astley - Never Gonna Give You Up",
	"description": "The official video for Never Gonna Give You Up by Rick Astley.\nReleased 1987.",
	"uploader": "Rick Astley",
	"duration": 213,
	"view_count": 1500000000,
	"upload_date": "20091025",
	"webpage_url": "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
	"channel_url": "https://www.youtube.com/channel/UCuAXFkgsw1L7xaCfnd5JJOw",
	"thumbnail": "https://i.ytimg.com/vi/dQw4w9WgXcQ/maxresdefault.jpg",
	"extractor": "youtube",
	"subtitles": {"en": [{"url": "https://example.com/sub-en.vtt", "ext": "vtt"}]},
	"automatic_captions": {"zh-Hans": [{"url": "https://example.com/sub-zh.vtt", "ext": "vtt"}]}
}`

func TestYtdlpFetcherCanHandle(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		url  string
		want bool
	}{
		{"youtube watch", "https://www.youtube.com/watch?v=dQw4w9WgXcQ", true},
		{"youtube watch m subdomain", "https://m.youtube.com/watch?v=dQw4w9WgXcQ", true},
		{"youtube shorts", "https://www.youtube.com/shorts/abc", true},
		{"youtube embed", "https://www.youtube.com/embed/abc", true},
		{"youtu.be short", "https://youtu.be/dQw4w9WgXcQ", true},
		{"vimeo plain", "https://vimeo.com/123456", true},
		{"vimeo channel", "https://vimeo.com/channels/staffpicks/123456", true},
		{"vimeo player", "https://player.vimeo.com/video/123456", true},
		{"bilibili", "https://www.bilibili.com/video/BV1xx411c7XW", true},
		{"twitch video", "https://www.twitch.tv/videos/12345", true},

		{"github url", "https://github.com/example/project", false},
		{"example url", "https://example.com/watch?v=foo", false},
		{"youtube homepage", "https://www.youtube.com/", false},
		{"youtube channel", "https://www.youtube.com/@somebody", false},
		{"twitch homepage", "https://www.twitch.tv/", false},
	}

	fetcher := NewYtdlpFetcher("", 0)
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := fetcher.CanHandle(c.url)
			if got != c.want {
				t.Fatalf("CanHandle(%q) = %v, want %v", c.url, got, c.want)
			}
		})
	}
}

func TestYtdlpFetcherFetchHappyPath(t *testing.T) {
	stub := writeYtdlpStub(t, ytdlpHappyJSON, "", 0)
	fetcher := NewYtdlpFetcher(stub, 5*time.Second)

	got, err := fetcher.Fetch(context.Background(), "https://www.youtube.com/watch?v=dQw4w9WgXcQ")
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}

	if got.FetcherType != "ytdlp" {
		t.Fatalf("FetcherType = %q, want ytdlp", got.FetcherType)
	}
	if got.Title != "Rick Astley - Never Gonna Give You Up" {
		t.Fatalf("Title = %q", got.Title)
	}
	if !strings.Contains(got.Body, "official video") {
		t.Fatalf("Body = %q, want substring 'official video'", got.Body)
	}

	wantImages := []string{"https://i.ytimg.com/vi/dQw4w9WgXcQ/maxresdefault.jpg"}
	if !reflect.DeepEqual(got.ImageURLs, wantImages) {
		t.Fatalf("ImageURLs = %v, want %v", got.ImageURLs, wantImages)
	}

	if uploader, ok := got.Metadata["uploader"].(string); !ok || uploader != "Rick Astley" {
		t.Fatalf("Metadata[uploader] = %v", got.Metadata["uploader"])
	}
	if dur, ok := got.Metadata["duration_seconds"].(int); !ok || dur != 213 {
		t.Fatalf("Metadata[duration_seconds] = %v, want 213", got.Metadata["duration_seconds"])
	}
	if views, ok := got.Metadata["view_count"].(int64); !ok || views != 1500000000 {
		t.Fatalf("Metadata[view_count] = %v", got.Metadata["view_count"])
	}
	if upload, ok := got.Metadata["upload_date"].(string); !ok || upload != "20091025" {
		t.Fatalf("Metadata[upload_date] = %v", got.Metadata["upload_date"])
	}
	if channel, ok := got.Metadata["channel_url"].(string); !ok || channel == "" {
		t.Fatalf("Metadata[channel_url] = %v", got.Metadata["channel_url"])
	}
	if thumb, ok := got.Metadata["thumbnail"].(string); !ok || thumb != wantImages[0] {
		t.Fatalf("Metadata[thumbnail] = %v", got.Metadata["thumbnail"])
	}
	if extractor, ok := got.Metadata["extractor"].(string); !ok || extractor != "youtube" {
		t.Fatalf("Metadata[extractor] = %v", got.Metadata["extractor"])
	}
	wantLangs := []string{"en", "zh-Hans"}
	gotLangs, ok := got.Metadata["subtitle_languages"].([]string)
	if !ok || !reflect.DeepEqual(gotLangs, wantLangs) {
		t.Fatalf("Metadata[subtitle_languages] = %v, want %v", got.Metadata["subtitle_languages"], wantLangs)
	}
}

func TestYtdlpFetcherFetchTruncatesBody(t *testing.T) {
	longDescription := strings.Repeat("中", 5000)
	json := `{"title": "x", "description": "` + longDescription + `", "extractor": "y"}`
	stub := writeYtdlpStub(t, json, "", 0)
	fetcher := NewYtdlpFetcher(stub, 5*time.Second)

	got, err := fetcher.Fetch(context.Background(), "https://www.youtube.com/watch?v=foo")
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	runes := []rune(got.Body)
	if len(runes) != 4000 {
		t.Fatalf("Body rune count = %d, want 4000", len(runes))
	}
	for _, r := range runes {
		if r != '中' {
			t.Fatalf("Body contains unexpected rune %q", r)
		}
	}
}

func TestYtdlpFetcherBinaryUnavailable(t *testing.T) {
	fetcher := NewYtdlpFetcher("/definitely/does/not/exist/yt-dlp", 5*time.Second)
	_, err := fetcher.Fetch(context.Background(), "https://www.youtube.com/watch?v=foo")
	if err == nil {
		t.Fatal("Fetch() error = nil, want binary unavailable")
	}
	if !strings.Contains(err.Error(), "binary unavailable") {
		t.Fatalf("Fetch() error = %v, want substring 'binary unavailable'", err)
	}
}

func TestYtdlpFetcherTimesOut(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "yt-dlp-slow.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nsleep 2\n"), 0o700); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	fetcher := NewYtdlpFetcher(path, 100*time.Millisecond)
	_, err := fetcher.Fetch(context.Background(), "https://www.youtube.com/watch?v=foo")
	if err == nil {
		t.Fatal("Fetch() error = nil, want timeout")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("Fetch() error = %v, want substring 'timed out'", err)
	}
}

func TestYtdlpFetcherNonZeroExit(t *testing.T) {
	stub := writeYtdlpStub(t, "", "ERROR: Video unavailable", 1)
	fetcher := NewYtdlpFetcher(stub, 5*time.Second)
	_, err := fetcher.Fetch(context.Background(), "https://www.youtube.com/watch?v=foo")
	if err == nil {
		t.Fatal("Fetch() error = nil, want exit error")
	}
	if !strings.Contains(err.Error(), "non-zero") {
		t.Fatalf("Fetch() error = %v, want substring 'non-zero'", err)
	}
	if !strings.Contains(err.Error(), "Video unavailable") {
		t.Fatalf("Fetch() error = %v, want stderr message included", err)
	}
}

func TestYtdlpFetcherMalformedJSON(t *testing.T) {
	stub := writeYtdlpStub(t, "this is not json", "", 0)
	fetcher := NewYtdlpFetcher(stub, 5*time.Second)
	_, err := fetcher.Fetch(context.Background(), "https://www.youtube.com/watch?v=foo")
	if err == nil {
		t.Fatal("Fetch() error = nil, want decode error")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Fatalf("Fetch() error = %v, want substring 'decode'", err)
	}
}

func TestYtdlpFetcherEmptyTitle(t *testing.T) {
	stub := writeYtdlpStub(t, `{"title": "", "description": "no title"}`, "", 0)
	fetcher := NewYtdlpFetcher(stub, 5*time.Second)
	_, err := fetcher.Fetch(context.Background(), "https://www.youtube.com/watch?v=foo")
	if err == nil {
		t.Fatal("Fetch() error = nil, want missing title")
	}
	if !strings.Contains(err.Error(), "missing title") {
		t.Fatalf("Fetch() error = %v, want substring 'missing title'", err)
	}
}

func TestYtdlpFetcherRejectsOversizedOutput(t *testing.T) {
	huge := strings.Repeat("x", 200)
	stub := writeYtdlpStub(t, `{"title":"`+huge+`"}`, "", 0)
	fetcher := NewYtdlpFetcher(stub, 5*time.Second)
	fetcher.MaxOutputSize = 50

	_, err := fetcher.Fetch(context.Background(), "https://www.youtube.com/watch?v=foo")
	if err == nil {
		t.Fatal("Fetch() error = nil, want oversized output")
	}
	if !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("Fetch() error = %v, want substring 'exceeded'", err)
	}
}

func TestYtdlpFetcherSkipsEmptyExtractor(t *testing.T) {
	stub := writeYtdlpStub(t, `{"title": "x", "description": "y"}`, "", 0)
	fetcher := NewYtdlpFetcher(stub, 5*time.Second)

	got, err := fetcher.Fetch(context.Background(), "https://www.youtube.com/watch?v=foo")
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if _, present := got.Metadata["extractor"]; present {
		t.Fatalf("Metadata[extractor] should be omitted when empty, got %v", got.Metadata["extractor"])
	}
}

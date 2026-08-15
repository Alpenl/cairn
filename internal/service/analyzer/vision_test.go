package analyzer

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"webtag/internal/fetcher"
)

const testVisionPNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="
const testVisionPNGDataURL = "data:image/png;base64," + testVisionPNGBase64

type visionRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn visionRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return fn(req) }

type visionLookupFunc func(context.Context, string) ([]net.IPAddr, error)

func (fn visionLookupFunc) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return fn(ctx, host)
}

func TestVisionRemoteImageIsFetchedByCairnAndInlinedForProvider(t *testing.T) {
	t.Parallel()

	pngBytes, err := base64.StdEncoding.DecodeString(testVisionPNGBase64)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	const remote = "https://images.example/screenshot.png?signed=query-secret#fragment-secret"
	visionCalls := 0
	visionClient := fetcher.NewHTTPClientWithOptions(fetcher.HTTPClientOptions{
		Client: &http.Client{Transport: visionRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			visionCalls++
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"image/png"}},
				Body:       io.NopCloser(strings.NewReader(string(pngBytes))),
				Request:    req,
			}, nil
		})},
		LookupIP: visionLookupFunc(func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
		}),
	})

	var providerBody string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		providerBody = string(data)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": `{"summary":"Screenshot summary","tags":["Vision"]}`}}},
		})
	}))
	defer provider.Close()

	a := NewOpenAIAnalyzer(OpenAIAnalyzerOptions{
		BaseURL: provider.URL, Model: "vision", HTTPClient: provider.Client(), VisionHTTPClient: visionClient,
		EmptyResponseRetries: 1, MinTags: 1,
	})
	_, err = a.Analyze(context.Background(), AnalyzeRequest{Content: fetcher.Content{
		URL: "https://example.com/post", Title: "Screenshot", Body: "Screenshot body", ImageURLs: []string{remote},
	}})
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	if visionCalls != 1 {
		t.Fatalf("Cairn image fetches=%d, want 1", visionCalls)
	}
	if strings.Contains(providerBody, remote) || strings.Contains(providerBody, "query-secret") || strings.Contains(providerBody, "images.example") {
		t.Fatalf("provider payload retained caller-controlled remote URL: %s", providerBody)
	}
	if !strings.Contains(providerBody, testVisionPNGDataURL) {
		t.Fatalf("provider payload missing validated inline image: %s", providerBody)
	}
}

func TestVisionUnsafeResolutionIsBlockedEvenWhenProviderAllowsUnsafeTargets(t *testing.T) {
	t.Parallel()

	imageTransportCalls := 0
	visionClient := fetcher.NewHTTPClientWithOptions(fetcher.HTTPClientOptions{
		Client: &http.Client{Transport: visionRoundTripFunc(func(*http.Request) (*http.Response, error) {
			imageTransportCalls++
			return nil, nil
		})},
		LookupIP: visionLookupFunc(func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("169.254.169.254")}}, nil
		}),
	})
	providerCalls := 0
	provider := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { providerCalls++ }))
	defer provider.Close()
	a := NewOpenAIAnalyzer(OpenAIAnalyzerOptions{
		BaseURL: provider.URL, Model: "vision", HTTPClient: provider.Client(), VisionHTTPClient: visionClient,
		EmptyResponseRetries: 1,
	})

	_, err := a.Analyze(context.Background(), AnalyzeRequest{Content: fetcher.Content{
		URL: "https://example.com/post", Body: "body", ImageURLs: []string{"https://public.example/image.png"},
	}})
	if err == nil {
		t.Fatal("Analyze() error = nil, want SSRF-safe image rejection")
	}
	if imageTransportCalls != 0 || providerCalls != 0 {
		t.Fatalf("image/provider calls=%d/%d, want 0/0", imageTransportCalls, providerCalls)
	}
	if strings.Contains(err.Error(), "public.example") {
		t.Fatalf("error leaked original image URL: %v", err)
	}
}

func TestVisionRejectsOversizedPixelDeclarationBeforeProvider(t *testing.T) {
	t.Parallel()

	data, err := base64.StdEncoding.DecodeString(testVisionPNGBase64)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	// Keep a complete, CRC-valid PNG container while changing only its IHDR
	// dimensions. This proves the pixel budget is the rejecting condition, not
	// a missing IDAT/IEND chunk or a bad checksum.
	binary.BigEndian.PutUint32(data[16:20], 100_000)
	binary.BigEndian.PutUint32(data[20:24], 100_000)
	binary.BigEndian.PutUint32(data[29:33], crc32.ChecksumIEEE(data[12:29]))
	if !isStaticPNG(data) {
		t.Fatal("oversized fixture is not a structurally valid static PNG")
	}
	_, err = validateVisionImage(data)
	if err == nil {
		t.Fatal("validateVisionImage() error = nil, want pixel-budget rejection")
	}
}

func TestVisionRejectsAnimatedPNGFrameDeclaration(t *testing.T) {
	t.Parallel()

	staticPNG, err := base64.StdEncoding.DecodeString(testVisionPNGBase64)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	// Insert an APNG acTL chunk immediately after IHDR. The ordinary PNG
	// decoder ignores this ancillary chunk, so the vision boundary must reject
	// it explicitly before the bytes are sent to the provider.
	const afterIHDR = 8 + 4 + 4 + 13 + 4
	chunk := make([]byte, 20)
	binary.BigEndian.PutUint32(chunk[0:4], 8)
	copy(chunk[4:8], "acTL")
	binary.BigEndian.PutUint32(chunk[8:12], 2)
	binary.BigEndian.PutUint32(chunk[12:16], 0)
	binary.BigEndian.PutUint32(chunk[16:20], crc32.ChecksumIEEE(chunk[4:16]))
	animated := append(append(append([]byte(nil), staticPNG[:afterIHDR]...), chunk...), staticPNG[afterIHDR:]...)

	if _, err := validateVisionImage(animated); err == nil {
		t.Fatal("validateVisionImage() error = nil, want animated-image rejection")
	}
}

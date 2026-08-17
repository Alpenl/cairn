package fetcher

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func TestBasicFetcherRejectsOversizedResponses(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html><body>" + strings.Repeat("A", 64) + "</body></html>"))
	}))
	defer server.Close()

	fetcher := NewBasicFetcher(NewHTTPClientWithOptions(HTTPClientOptions{Client: server.Client(), AllowUnsafeTargets: true}))
	fetcher.MaxBytes = 16

	_, err := fetcher.Fetch(context.Background(), server.URL)
	if err == nil {
		t.Fatal("Fetch() error = nil, want oversized response error")
	}
	if !strings.Contains(err.Error(), "response too large") {
		t.Fatalf("Fetch() error = %v, want oversized response error", err)
	}
}

func TestBasicFetcherRetriesTransientHTTPFailures(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html><head><title>Recovered</title></head><body>Recovered body content for parser.</body></html>"))
	}))
	defer server.Close()

	fetcher := NewBasicFetcher(NewHTTPClientWithOptions(HTTPClientOptions{Client: server.Client(), AllowUnsafeTargets: true}))

	got, err := fetcher.Fetch(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("HTTP calls = %d, want 2", calls.Load())
	}
	if got.Title != "Recovered" {
		t.Fatalf("Title = %q, want Recovered", got.Title)
	}
}

func TestBasicFetcherRejectsBinaryContentTypes(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("not really a png"))
	}))
	defer server.Close()

	fetcher := NewBasicFetcher(NewHTTPClientWithOptions(HTTPClientOptions{Client: server.Client(), AllowUnsafeTargets: true}))

	_, err := fetcher.Fetch(context.Background(), server.URL)
	if err == nil {
		t.Fatal("Fetch() error = nil, want binary content-type rejection")
	}
	if !strings.Contains(err.Error(), "content-type") {
		t.Fatalf("Fetch() error = %v, want content-type error", err)
	}
}

func TestBasicFetcherAllowsPlainTextHTMLFallback(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("<html><head><title>Plain Label</title></head><body>Readable content still available.</body></html>"))
	}))
	defer server.Close()

	fetcher := NewBasicFetcher(NewHTTPClientWithOptions(HTTPClientOptions{Client: server.Client(), AllowUnsafeTargets: true}))

	got, err := fetcher.Fetch(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("Fetch() error = %v, want success for text/plain HTML fallback", err)
	}
	if got.Body == "" {
		t.Fatal("Body = empty, want extracted content")
	}
}

func TestBasicFetcherPrefersMetadataTitleOverGenericHTMLTitle(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><head><title>Home</title><meta property="og:title" content="Go Parser Guide"></head><body><article><h1>Ignored Heading</h1><p>Useful parser body content.</p></article></body></html>`))
	}))
	defer server.Close()

	fetcher := NewBasicFetcher(NewHTTPClientWithOptions(HTTPClientOptions{Client: server.Client(), AllowUnsafeTargets: true}))

	got, err := fetcher.Fetch(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if got.Title != "Go Parser Guide" {
		t.Fatalf("Title = %q, want Go Parser Guide", got.Title)
	}
}

func TestBasicFetcherKeepsReadableDocumentStructure(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><head><title>Structured Guide</title></head><body><article>
			<h1>Structured Guide</h1><h2>Overview</h2>
			<p>First paragraph.</p>
			<ul><li>One</li><li>Two</li></ul>
			<pre><code>fmt.Println(&quot;ok&quot;)</code></pre>
		</article></body></html>`))
	}))
	defer server.Close()

	fetcher := NewBasicFetcher(NewHTTPClientWithOptions(HTTPClientOptions{Client: server.Client(), AllowUnsafeTargets: true}))
	got, err := fetcher.Fetch(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if !strings.Contains(got.HTML, "<h2") || !strings.Contains(got.HTML, "<ul") || !strings.Contains(got.HTML, "<pre") {
		t.Fatalf("HTML = %q, want readability structure for subheadings, lists, and code", got.HTML)
	}
	if !strings.Contains(got.Body, "First paragraph.\n") {
		t.Fatalf("Body = %q, want readable block boundaries", got.Body)
	}
}

func TestBasicFetcherFallsBackToHeadingWhenTitleIsGeneric(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><head><title>Index</title></head><body><main><h1>Readable Fallback Heading</h1><p>Useful parser body content.</p></main></body></html>`))
	}))
	defer server.Close()

	fetcher := NewBasicFetcher(NewHTTPClientWithOptions(HTTPClientOptions{Client: server.Client(), AllowUnsafeTargets: true}))

	got, err := fetcher.Fetch(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if got.Title != "Readable Fallback Heading" {
		t.Fatalf("Title = %q, want Readable Fallback Heading", got.Title)
	}
}

// mp.weixin.qq.com hides #js_content until its own script reveals it, which
// used to cost us the entire article: readability and the HTML→document
// conversion both skip invisible subtrees, so a live article extracted down
// to its byline. Guard the reveal, the lazy-image repair, and the furniture
// removal together — they are the difference between a saved article and a
// saved title.
func TestWeChatFetcherRevealsTheHiddenArticleBody(t *testing.T) {
	t.Parallel()

	fixture, err := os.ReadFile("testdata/wechat_article.html")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		header := make(http.Header)
		header.Set("Content-Type", "text/html; charset=utf-8")
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     header,
			Body:       io.NopCloser(strings.NewReader(string(fixture))),
			Request:    req,
		}, nil
	})}
	fetcher := NewWeChatFetcher(NewHTTPClientWithOptions(HTTPClientOptions{
		Client:             client,
		AllowUnsafeTargets: true,
	}))

	got, err := fetcher.Fetch(context.Background(), "https://mp.weixin.qq.com/s/fixture-signature")
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}

	if got.Title != "把懒加载的图片交给读者之前" {
		t.Errorf("Title = %q, want 把懒加载的图片交给读者之前", got.Title)
	}
	if got.FetcherType != "wechat" {
		t.Errorf("FetcherType = %q, want wechat", got.FetcherType)
	}
	for _, want := range []string{
		"页面把正文交给脚本",
		"地址写在 data-src 上",
		"把容器钉住",
		"行号并不写在文档里",
	} {
		if !strings.Contains(got.Body, want) {
			t.Errorf("Body missing article text %q (body_len=%d)", want, len(got.Body))
		}
	}

	// Lazy-loaded illustrations reach the saved document with an address.
	for _, want := range []string{
		`src="https://mmbiz.qpic.cn/mmbiz_png/fixture-illustration-one/640?wx_fmt=png"`,
		`src="https://mmbiz.qpic.cn/mmbiz_jpg/fixture-illustration-two/640?wx_fmt=jpeg"`,
	} {
		if !strings.Contains(got.HTML, want) {
			t.Errorf("HTML missing repaired image source %q", want)
		}
	}

	// The container is revealed, but styling that is not about hiding stays.
	for _, hiding := range []string{"visibility", "opacity"} {
		if strings.Contains(got.HTML, hiding) {
			t.Errorf("HTML still carries the %q declaration", hiding)
		}
	}
	if !strings.Contains(got.HTML, "font-size: 16px") {
		t.Error("HTML dropped inline styling unrelated to hiding")
	}

	for _, unwanted := range []string{
		"扫描二维码关注",
		"喜欢作者，赞赏支持",
		"关注测试公众号，查看更多内容",
		"code-snippet__line-index",
		"window.__wx_reveal__",
		"line-height: 1.75",
		"写留言",
	} {
		bodyHas := strings.Contains(got.Body, unwanted)
		htmlHas := strings.Contains(got.HTML, unwanted)
		if bodyHas || htmlHas {
			t.Errorf("extracted content contains furniture %q (body=%v html=%v)", unwanted, bodyHas, htmlHas)
		}
	}
}

func TestBasicFetcherDecodesV2EXAndExcludesReplies(t *testing.T) {
	t.Parallel()

	fixture, err := os.ReadFile("testdata/v2ex_topic.html")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	repeatedReply := `<div id="r_fixture" class="cell"><a href="/member/reply-user"><strong>reply-user</strong></a><span class="ago">13 days ago</span><div class="reply_content">This is a reader reply discussing rollout strategy, testing, architecture, monitoring, performance, and maintenance. It is deliberately long enough to reproduce a discussion page where the aggregate reply thread outweighs the original post.</div></div>`
	page := strings.Replace(string(fixture), "<!-- repeated-replies -->", strings.Repeat(repeatedReply, 80), 1)
	page = strings.Replace(page, "<title>", strings.Repeat("<!-- ascii head padding -->", 80)+"<title>", 1)

	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		header := make(http.Header)
		header.Set("Content-Type", "text/html; charset=UTF-8")
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     header,
			Body:       io.NopCloser(strings.NewReader(page)),
			Request:    req,
		}, nil
	})}
	fetcher := NewBasicFetcher(NewHTTPClientWithOptions(HTTPClientOptions{
		Client:             client,
		AllowUnsafeTargets: true,
	}))

	got, err := fetcher.Fetch(context.Background(), "https://v2ex.com/t/1224558")
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	for _, want := range []string{"今年年初", "大量核心代码", "问问各位大神"} {
		if !strings.Contains(got.Body, want) {
			t.Errorf("Body missing original-post text %q (body_len=%d)", want, len(got.Body))
		}
	}
	for _, unwanted := range []string{
		"ä»Šå¹´", "ï¼Œ", "icanfork", "iv8d", "sentinelK", "196 replies", "13 days ago",
	} {
		bodyHas := strings.Contains(got.Body, unwanted)
		htmlHas := strings.Contains(got.HTML, unwanted)
		if bodyHas || htmlHas {
			t.Errorf("extracted content contains %q (body=%v html=%v)", unwanted, bodyHas, htmlHas)
		}
	}
}

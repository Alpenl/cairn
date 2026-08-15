package fetcher

// Benchmark 覆盖 LightFetcher 抓回 body 后的"解析 + 截断"段——也就是
// extractReadableContent (dom.Parse + readability + goquery) 加 truncateRunes。
// 网络层（HTTP / Range / DoWithRetry）刻意不进 benchmark：它会引入 httptest
// server 的 syscall 噪声，掩盖 CPU 层的真实开销。
//
// 这两个 benchmark 直接喂截断后的 HTML byte slice 给 extractReadableContent，
// 等价于 LightFetcher.Fetch 的最后两行（解析 + 截断），是 light 路径里
// 唯一 CPU-bound 的步骤。

import (
	"strings"
	"testing"
)

// benchTruncatedHTML 模拟 Range 请求只拿到前 16KB 的常见情形——HTML 头部完整，
// body 在某个 <p> 中间被硬切断。dom.Parse 会自动合上未闭合标签，readability
// 在残缺树上跑 extraction，恰是 light 路径的真实负载。
//
// 用 strings.Repeat 把 body 填到 ~16KB；末尾故意不闭合，模拟 Range 截断。
var benchTruncatedHTML = []byte(`<!DOCTYPE html>
<html>
<head>
  <meta property="og:title" content="Project announcement: open-sourcing FrameworkX">
  <meta property="og:description" content="Today we announce Project, our open-source FrameworkX for knowledge bases.">
  <title>Project announcement</title>
</head>
<body>
  <article>
    <h1>Project announcement: open-sourcing FrameworkX</h1>
    <p>We are pleased to announce Project today. The codebase, called FrameworkX,
    has been released under an open-source license. It is built around a RAG
    architecture with first-class support for multi-format document ingestion.</p>
    <p>` + strings.Repeat("Filler paragraph with some technical-looking words like Postgres, gRPC, gin, pgx, sonic. ", 150) + `</p>
    <p>This last paragraph is intentionally not closed`)

// BenchmarkLightFetcher_ParseTruncated 衡量截断 HTML 的解析吞吐。
// 跑的就是 LightFetcher.Fetch 里那两行（extractReadableContent + truncateRunes）—
// 整个 light 路径中唯一与 body size 相关的 CPU 工作量。
func BenchmarkLightFetcher_ParseTruncated(b *testing.B) {
	const maxChars = 1000
	const url = "https://example.com/foo/bar"

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		body, title, _, _ := extractReadableContent(url, benchTruncatedHTML, "text/html; charset=utf-8")
		body = truncateRunes(body, maxChars)
		if body == "" && title == "" {
			b.Fatal("empty extraction")
		}
	}
}

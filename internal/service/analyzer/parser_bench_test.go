package analyzer

// Benchmark 覆盖 parseAnalysisResponse 的两条典型解析路径：
//
//   1. Happy path——LLM 直接给一段干净 JSON，整个字符串一次就解码成功，jsonCandidates
//      只产出一个候选，validateAnalysisResponse 一次通过。
//   2. Noisy markdown fences——LLM 把 JSON 包在 ```json ... ``` 里、前后还带中文
//      旁白。jsonCandidates 会走 regex + 手写大括号扫描器，产出多个候选并逐个
//      decode，是这个解析器最耗 CPU 的形态。
//
// 解析层在每次 LLM 调用后都会被调用一次，吞吐受这个函数的 ns/op 直接影响。

import (
	"testing"
)

// happyPathResponse 用典型规模的 summary（~40 字）+ 5 个 tag 构造一段干净 JSON。
// 既能验证 fast path 的 baseline，又不会因为 summary 过短让常数项失真。
var happyPathResponse = `{"summary":"这篇文章讨论了检索增强生成（RAG）在企业知识库中的落地经验。","tags":["RAG","LLM","知识库","Embedding","架构"]}`

// noisyMarkdownResponse 模拟 LLM 不守约定时的常见返回：前置解释、JSON 包在
// fenced code block 里、尾巴还有客套话。覆盖 jsonBlockRE / jsonObjectRE / 手写
// 扫描器三条候选链路。
var noisyMarkdownResponse = "好的，根据正文内容我整理出如下结果：\n\n```json\n" +
	`{
  "summary": "这篇文章讨论了检索增强生成（RAG）在企业知识库中的落地经验。",
  "tags": ["RAG", "LLM", "知识库", "Embedding", "架构"]
}` +
	"\n```\n\n以上就是我的分析结果，如有补充欢迎告知。"

// newBenchAnalyzer 复用真实生产配置的字符上限和标签数量约束，避免
// validate 阶段被宽松阈值短路过头，benchmark 结果失真。
func newBenchAnalyzer() *OpenAIAnalyzer {
	return NewOpenAIAnalyzer(OpenAIAnalyzerOptions{
		BaseURL:         "https://example.com",
		APIKey:          "bench-key",
		Model:           "gpt-test",
		MaxSummaryChars: 200,
		MinTags:         3,
		MaxTags:         8,
		MaxTagChars:     20,
	})
}

// BenchmarkParseAnalysisResponse_HappyPath 衡量"LLM 给的就是干净 JSON"的最佳路径。
// jsonCandidates 一次命中，decode + validate 一次通过。
func BenchmarkParseAnalysisResponse_HappyPath(b *testing.B) {
	a := newBenchAnalyzer()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := a.parseAnalysisResponse(happyPathResponse); err != nil {
			b.Fatalf("parse: %v", err)
		}
	}
}

// BenchmarkParseAnalysisResponse_NoisyMarkdownFences 衡量带 markdown 包装 + 旁白
// 的情况——这是生产里 ~30% LLM 响应会走的分支，正则匹配 + 手写扫描器都会跑。
func BenchmarkParseAnalysisResponse_NoisyMarkdownFences(b *testing.B) {
	a := newBenchAnalyzer()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := a.parseAnalysisResponse(noisyMarkdownResponse); err != nil {
			b.Fatalf("parse: %v", err)
		}
	}
}

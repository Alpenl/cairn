// Command eval is the WebTag product-evaluation harness CLI.
//
// Unlike `go test`, eval answers "how good is the tag output" rather
// than "is the function correct". It runs the production fetcher +
// analyzer stack across a matrix of (cases × fetchers × prompts ×
// models) and produces a markdown report the operator reviews by
// hand.
//
// Usage examples:
//
//	# minimal: one case file, Grok URL-direct fetch, production prompt, single model
//	eval run --case evals/cases/seed.yaml
//
//	# cross-fetcher comparison on Chinese articles
//	eval run --case evals/cases/chinese_articles.yaml \
//	         --fetcher basic,light,wechat \
//	         --out evals/results/$(date +%F)_zh.md
//
//	# compare the production prompt with a historical baseline
//	eval run --case evals/cases/seed.yaml \
//	         --prompt production,legacy=evals/prompts/legacy_50_chars.txt \
//	         --model grok-4.3-fast
//
// Environment: AI_BASE_URL, AI_API_KEY, AI_MODEL are required
// (eval reuses the production analyzer). Optional: GITHUB_TOKEN,
// YTDLP_BINARY_PATH, YTDLP_TIMEOUT_MS for the matching fetchers.
package main

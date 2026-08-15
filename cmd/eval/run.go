package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"webtag/internal/eval"
	"webtag/internal/fetcher"
	"webtag/internal/service/analyzer"
)

func runCmd(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	var (
		caseSpec   = fs.String("case", "evals/cases", "Path to a case YAML file, directory, or glob (multiple via comma).")
		fetcherCSV = fs.String("fetcher", "grok", "Comma-separated fetcher names: grok, basic, light, wechat, github, arxiv, ytdlp, router.")
		promptCSV  = fs.String("prompt", "production", "Comma-separated prompt specs: production, prompt files, or name=path.")
		modelCSV   = fs.String("model", "", "Comma-separated model names (overrides AI_MODEL when set).")
		outPath    = fs.String("out", "", "Output path (default: stdout).")
		format     = fs.String("format", "md", "Report format: md | json | csv.")
		timeoutSec = fs.Int("timeout-sec", 60, "Per-cell (fetch + analyze) timeout.")
		useJudge   = fs.Bool("judge", false, "Score each cell with the LLM judge (extra LLM call per cell).")
		judgeModel = fs.String("judge-model", "", "Model name for the LLM judge (defaults to AI_MODEL).")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	envCfg, err := loadEnvConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "eval:", err)
		return 1
	}

	cases, err := loadCases(*caseSpec)
	if err != nil {
		fmt.Fprintln(os.Stderr, "eval:", err)
		return 1
	}
	fetchers, err := parseFetchers(*fetcherCSV)
	if err != nil {
		fmt.Fprintln(os.Stderr, "eval:", err)
		return 1
	}
	prompts, err := eval.LoadPromptVariants(splitCSV(*promptCSV))
	if err != nil {
		fmt.Fprintln(os.Stderr, "eval:", err)
		return 1
	}
	models := splitCSV(*modelCSV)
	if len(models) == 0 {
		models = []string{envCfg.Model}
	}

	httpClient := fetcher.NewHTTPClient(&http.Client{Timeout: time.Duration(*timeoutSec) * time.Second})
	fetcherOpts := eval.FetcherBuildOptions{
		GitHubToken:     envCfg.GitHubToken,
		YtdlpBinaryPath: envCfg.YtdlpBinaryPath,
		YtdlpTimeout:    envCfg.YtdlpTimeout,
		HTTPClient:      httpClient,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	var judge eval.Judge
	if *useJudge {
		jm := strings.TrimSpace(*judgeModel)
		if jm == "" {
			jm = envCfg.Model
		}
		judge = eval.NewHTTPJudge(eval.HTTPJudgeOptions{
			BaseURL:    envCfg.BaseURL,
			APIKey:     envCfg.APIKey,
			Model:      jm,
			HTTPClient: httpClient.Raw(),
			Timeout:    time.Duration(*timeoutSec) * time.Second,
		})
	}

	result, err := eval.Run(ctx, eval.RunConfig{
		Cases:    cases,
		Fetchers: fetchers,
		Prompts:  prompts,
		Models:   models,
		BuildFetcher: func(n eval.FetcherName) (fetcher.Fetcher, error) {
			return eval.BuildFetcher(n, fetcherOpts)
		},
		BuildAnalyzer: func(model string) *analyzer.OpenAIAnalyzer {
			return analyzer.NewOpenAIAnalyzer(analyzer.OpenAIAnalyzerOptions{
				BaseURL:        envCfg.BaseURL,
				APIKey:         envCfg.APIKey,
				Model:          model,
				HTTPClient:     httpClient.Raw(),
				RequestTimeout: time.Duration(*timeoutSec) * time.Second,
			})
		},
		Judge:          judge,
		PerCallTimeout: time.Duration(*timeoutSec) * time.Second,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "eval:", err)
		return 1
	}

	out := os.Stdout
	if strings.TrimSpace(*outPath) != "" {
		f, err := os.Create(*outPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "eval:", err)
			return 1
		}
		defer f.Close()
		out = f
	}

	if err := renderResult(out, *format, result); err != nil {
		fmt.Fprintln(os.Stderr, "eval:", err)
		return 1
	}
	if *outPath != "" {
		fmt.Fprintln(os.Stderr, "wrote", *outPath)
	}
	return 0
}

func renderResult(out *os.File, format string, result eval.RunResult) error {
	switch strings.ToLower(format) {
	case "md", "markdown":
		return eval.RenderMarkdown(out, result)
	case "json":
		return eval.RenderJSON(out, result)
	case "csv":
		return eval.RenderCSV(out, result)
	default:
		return fmt.Errorf("unknown --format %q (use md, json, csv)", format)
	}
}

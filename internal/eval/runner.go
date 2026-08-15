package eval

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"webtag/internal/fetcher"
	"webtag/internal/service/analyzer"
	"webtag/internal/summarypolicy"
)

// PromptVariant pairs an operator-facing name with the system
// prompt text. The name shows in reports; the text replaces the
// production defaultSystemPrompt via AnalyzeRequest.SystemPromptOverride.
type PromptVariant struct {
	Name string
	Body string
}

// LoadPromptVariants reads `name=path` pairs (or bare paths,
// inferring name from the filename without extension). A directory
// path expands to all *.txt files inside.
func LoadPromptVariants(specs []string) ([]PromptVariant, error) {
	if len(specs) == 0 {
		return nil, fmt.Errorf("eval: at least one prompt variant required")
	}
	out := make([]PromptVariant, 0, len(specs))
	for _, spec := range specs {
		if strings.EqualFold(strings.TrimSpace(spec), "production") {
			out = append(out, PromptVariant{Name: "production"})
			continue
		}
		name, path := splitPromptSpec(spec)
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("eval: prompt %s: %w", path, err)
		}
		if info.IsDir() {
			entries, err := os.ReadDir(path)
			if err != nil {
				return nil, fmt.Errorf("eval: read prompt dir %s: %w", path, err)
			}
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".txt") {
					continue
				}
				body, err := os.ReadFile(filepath.Join(path, e.Name()))
				if err != nil {
					return nil, fmt.Errorf("eval: read prompt %s: %w", e.Name(), err)
				}
				out = append(out, PromptVariant{
					Name: strings.TrimSuffix(e.Name(), ".txt"),
					Body: string(body),
				})
			}
			continue
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("eval: read prompt %s: %w", path, err)
		}
		if name == "" {
			name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		}
		out = append(out, PromptVariant{Name: name, Body: string(body)})
	}
	return out, nil
}

// splitPromptSpec accepts "name=path" or bare "path".
func splitPromptSpec(spec string) (name, path string) {
	if eq := strings.IndexByte(spec, '='); eq > 0 {
		return spec[:eq], spec[eq+1:]
	}
	return "", spec
}

// AnalyzerBuilder constructs an *analyzer.OpenAIAnalyzer for a
// given model name. The harness needs a fresh analyzer per model so
// it can run gpt-4.1-mini and gpt-4o side by side; everything else
// (BaseURL, APIKey, HTTPClient) stays constant across the run.
type AnalyzerBuilder func(model string) *analyzer.OpenAIAnalyzer

// RunConfig is the full input to a single matrix run.
type RunConfig struct {
	Cases    []Case
	Fetchers []FetcherName
	Prompts  []PromptVariant
	Models   []string

	BuildFetcher  func(FetcherName) (fetcher.Fetcher, error)
	BuildAnalyzer AnalyzerBuilder

	// Judge, when non-nil, scores every successful cell with the
	// LLM judge. Failed cells (fetch / analyze errored) are skipped
	// because there are no tags to judge. Judge errors land in the
	// cell's JudgeReason field as "error: ..." rather than
	// aborting the matrix.
	Judge Judge

	// PerCallTimeout caps each (fetch + analyze) cell. Cells
	// exceeding it are recorded with err="timeout" so the matrix
	// still completes and the operator sees which combinations
	// hang. 60s is a sane default for live web pages + cheap LLM.
	PerCallTimeout time.Duration
}

// CellResult is the outcome of running one (case, fetcher, prompt,
// model) tuple. Filled progressively — Fetch* fields are populated
// even if Analyze errors out, so a partial trace still surfaces.
type CellResult struct {
	CaseID            string
	URL               string
	Fetcher           FetcherName
	Prompt            string
	Model             string
	FetchOK           bool
	FetchDurationMS   int64
	Title             string
	BodyChars         int
	AnalyzeOK         bool
	AnalyzeDurationMS int64
	Summary           string
	Tags              []string
	Rule              RuleScore
	SummaryProfile    string
	SummaryRule       summarypolicy.Assessment
	ContentContract   ContentContractAssessment
	JudgeScore        float64 // populated when --judge is set
	JudgeSummaryScore float64
	JudgeTagScore     float64
	JudgeReason       string
	Err               string
}

// RunResult bundles the matrix output plus enough provenance for a
// later diff or replay (git commit, model list, prompt names).
type RunResult struct {
	RunID     string
	StartedAt time.Time
	Duration  time.Duration
	Config    RunConfigSummary
	Cells     []CellResult
}

// RunConfigSummary mirrors RunConfig minus the closures so it can
// be marshalled to JSON / printed in reports.
type RunConfigSummary struct {
	Fetchers []FetcherName
	Prompts  []string
	Models   []string
}

// Run iterates the full (cases × fetchers × prompts × models)
// matrix sequentially. Sequential is intentional: parallel runs
// would race the rate limits on the LLM upstream and the per-domain
// politeness rules in the fetcher layer (think Wikidata, GitHub).
// The harness is a tool the operator runs by hand, not a hot path.
func Run(ctx context.Context, cfg RunConfig) (RunResult, error) {
	if cfg.BuildFetcher == nil || cfg.BuildAnalyzer == nil {
		return RunResult{}, fmt.Errorf("eval: Run requires BuildFetcher and BuildAnalyzer")
	}
	if len(cfg.Cases) == 0 {
		return RunResult{}, fmt.Errorf("eval: no cases supplied")
	}
	if len(cfg.Fetchers) == 0 {
		return RunResult{}, fmt.Errorf("eval: no fetchers supplied")
	}
	if len(cfg.Prompts) == 0 {
		return RunResult{}, fmt.Errorf("eval: no prompts supplied")
	}
	if len(cfg.Models) == 0 {
		return RunResult{}, fmt.Errorf("eval: no models supplied")
	}
	perCall := cfg.PerCallTimeout
	if perCall <= 0 {
		perCall = 60 * time.Second
	}

	// Build the analyzers up front, one per model. Constructing
	// inside the loop would repeat the HTTP client setup tax for
	// every cell of the matrix.
	analyzers := make(map[string]*analyzer.OpenAIAnalyzer, len(cfg.Models))
	for _, m := range cfg.Models {
		analyzers[m] = cfg.BuildAnalyzer(m)
	}

	started := time.Now()
	res := RunResult{
		RunID:     started.UTC().Format("20060102T150405Z"),
		StartedAt: started,
		Config: RunConfigSummary{
			Fetchers: cfg.Fetchers,
			Prompts:  promptNames(cfg.Prompts),
			Models:   cfg.Models,
		},
	}

	for _, c := range cfg.Cases {
		for _, fn := range cfg.Fetchers {
			var content fetcher.Content
			var fetchErr error
			var fetchMS int64
			if fn == FetcherGrok {
				content = fetcher.Content{URL: c.URL, FetcherType: string(FetcherGrok)}
			} else {
				fetcherImpl, err := cfg.BuildFetcher(fn)
				if err != nil {
					res.Cells = append(res.Cells, errorCell(c, fn, "", "", err))
					continue
				}
				content, fetchErr, fetchMS = timedFetch(ctx, fetcherImpl, c.URL, perCall)
			}
			profile := summarypolicy.For(summarypolicy.Input{
				URL:         c.URL,
				ContentType: c.ContentType,
				FetcherType: content.FetcherType,
			})
			var judgeSource fetcher.Content
			judgeSourceLoaded := false
			for _, pv := range cfg.Prompts {
				for _, model := range cfg.Models {
					cell := CellResult{
						CaseID:          c.ID,
						URL:             c.URL,
						Fetcher:         fn,
						Prompt:          pv.Name,
						Model:           model,
						FetchOK:         fetchErr == nil,
						FetchDurationMS: fetchMS,
						Title:           content.Title,
						BodyChars:       len([]rune(content.Body)),
						SummaryProfile:  profile.Name,
					}
					if fetchErr != nil {
						cell.Err = "fetch: " + fetchErr.Error()
						cell.Rule = ScoreTags(nil, c.Expected)
						res.Cells = append(res.Cells, cell)
						continue
					}
					cellContent := content
					ar, analyzeErr, analyzeMS := timedAnalyze(ctx, analyzers[model], analyzer.AnalyzeRequest{
						Content:              content,
						ContentType:          c.ContentType,
						URLDirect:            fn == FetcherGrok,
						SystemPromptOverride: pv.Body,
					}, perCall)
					cell.AnalyzeDurationMS = analyzeMS
					if fn == FetcherGrok && grokNeedsLocalFallback(ar, analyzeErr) {
						fallbackFetcher, buildErr := cfg.BuildFetcher(FetcherRouter)
						if buildErr != nil {
							cell.FetchOK = false
							cell.Err = "fallback fetcher: " + buildErr.Error()
							cell.Rule = ScoreTags(nil, c.Expected)
							res.Cells = append(res.Cells, cell)
							continue
						}
						fallbackContent, fallbackErr, fallbackFetchMS := timedFetch(ctx, fallbackFetcher, c.URL, perCall)
						cell.FetchDurationMS += fallbackFetchMS
						cell.FetchOK = fallbackErr == nil
						cell.Title = fallbackContent.Title
						cell.BodyChars = len([]rune(fallbackContent.Body))
						if fallbackErr != nil {
							cell.Err = "fallback fetch: " + fallbackErr.Error()
							cell.Rule = ScoreTags(nil, c.Expected)
							res.Cells = append(res.Cells, cell)
							continue
						}
						cellContent = fallbackContent
						ar, analyzeErr, analyzeMS = timedAnalyze(ctx, analyzers[model], analyzer.AnalyzeRequest{
							Content:              fallbackContent,
							ContentType:          c.ContentType,
							SystemPromptOverride: pv.Body,
						}, perCall)
						cell.AnalyzeDurationMS += analyzeMS
					}
					if analyzeErr != nil {
						cell.Err = "analyze: " + analyzeErr.Error()
						cell.Rule = ScoreTags(nil, c.Expected)
						res.Cells = append(res.Cells, cell)
						continue
					}
					cell.AnalyzeOK = true
					if strings.TrimSpace(ar.Title) != "" {
						cell.Title = ar.Title
					}
					cell.Summary = ar.Summary
					cell.Tags = ar.Tags
					cell.Rule = ScoreTags(ar.Tags, c.Expected)
					cell.SummaryRule = summarypolicy.Assess(ar.Summary, profile)
					cell.ContentContract = AssessContentContract(cell.Title, ar.Summary, c.Expected)
					if cfg.Judge != nil {
						judgeBody := cellContent.Body
						if fn == FetcherGrok && strings.TrimSpace(judgeBody) == "" {
							if !judgeSourceLoaded {
								judgeSourceLoaded = true
								if sourceFetcher, buildErr := cfg.BuildFetcher(FetcherRouter); buildErr == nil {
									if fetched, sourceErr, _ := timedFetch(ctx, sourceFetcher, c.URL, perCall); sourceErr == nil {
										judgeSource = fetched
									}
								}
							}
							judgeBody = judgeSource.Body
						}
						judgeCtx, jc := context.WithTimeout(ctx, perCall)
						verdict, jerr := cfg.Judge.JudgeCell(judgeCtx, JudgeInput{
							URL:                 c.URL,
							Title:               cell.Title,
							BodyPreview:         judgeBody,
							Summary:             ar.Summary,
							SummaryProfile:      profile.Name,
							SummaryRequirements: profile.PromptInstructions(),
							Tags:                ar.Tags,
						})
						jc()
						if jerr != nil {
							cell.JudgeReason = "error: " + jerr.Error()
						} else {
							cell.JudgeScore = verdict.Score
							cell.JudgeSummaryScore = verdict.SummaryScore
							cell.JudgeTagScore = verdict.TagScore
							cell.JudgeReason = verdict.Reason
						}
					}
					res.Cells = append(res.Cells, cell)
				}
			}
		}
	}

	res.Duration = time.Since(started)
	return res, nil
}

func grokNeedsLocalFallback(result analyzer.AnalysisResult, err error) bool {
	return err != nil || !result.Accessible || strings.TrimSpace(result.Title) == ""
}

func errorCell(c Case, fn FetcherName, prompt, model string, err error) CellResult {
	return CellResult{
		CaseID:  c.ID,
		URL:     c.URL,
		Fetcher: fn,
		Prompt:  prompt,
		Model:   model,
		Err:     err.Error(),
		Rule:    ScoreTags(nil, c.Expected),
	}
}

func timedFetch(ctx context.Context, f fetcher.Fetcher, url string, timeout time.Duration) (fetcher.Content, error, int64) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	content, err := f.Fetch(ctx, url)
	ms := time.Since(start).Milliseconds()
	if err != nil && errors.Is(err, context.DeadlineExceeded) {
		return content, fmt.Errorf("timeout after %s", timeout), ms
	}
	return content, err, ms
}

func timedAnalyze(ctx context.Context, a *analyzer.OpenAIAnalyzer, req analyzer.AnalyzeRequest, timeout time.Duration) (analyzer.AnalysisResult, error, int64) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	res, err := a.Analyze(ctx, req)
	ms := time.Since(start).Milliseconds()
	if err != nil && errors.Is(err, context.DeadlineExceeded) {
		return res, fmt.Errorf("timeout after %s", timeout), ms
	}
	return res, err, ms
}

func promptNames(variants []PromptVariant) []string {
	out := make([]string, len(variants))
	for i, v := range variants {
		out[i] = v.Name
	}
	return out
}

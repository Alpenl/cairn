// Package eval — product/functional evaluation harness.
//
// The eval harness sits *outside* unit testing: instead of "does
// function X return Y for input Z" it answers "how good is the tag
// output when I swap fetcher A for B?", "does prompt variant V2
// produce more specific tags than the production prompt?", "is
// gpt-4o noticeably better than gpt-4.1-mini on Chinese articles?".
//
// Cases are YAML files under evals/cases/. Each case is one URL +
// a description + an `expected` block that scopes the rule-based
// scorer. The harness composes (cases × fetchers × prompts × models),
// runs each combination, and produces a markdown / JSON / CSV report
// the operator reviews by hand or feeds into a diff against a
// baseline.
//
// This file owns the YAML schema definitions and the loader. Other
// files in this package: runner (execution), scorer (rule-based
// scoring), reporter (markdown/json/csv output), judge (optional
// LLM scoring), diff (compare two runs).
package eval

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// CaseFileSchema is the YAML envelope version we accept. Bumping it
// is the operator's signal that a breaking field change happened —
// older files still parse but a CLI run prints a "schema mismatch"
// hint if the value drifts. Backward compatibility within a major
// version is preserved by additive fields only.
const CaseFileSchema = "webtag.eval/v1"

// CaseFile is the on-disk envelope. Multiple cases per file lets
// operators group by theme (chinese_articles.yaml, github_repos.yaml,
// wechat.yaml) without juggling many small files.
type CaseFile struct {
	Schema string `yaml:"schema"`
	Cases  []Case `yaml:"cases"`
}

// Case is one evaluation scenario: a URL plus the operator's
// expectations about the tags an ideal fetcher + analyzer pipeline
// should produce.
//
// Expected is intentionally a separate sub-struct so callers can
// extend the scoring contract (e.g. add expected.summary_keywords)
// without touching the top-level Case shape.
type Case struct {
	ID          string       `yaml:"id"`
	URL         string       `yaml:"url"`
	Description string       `yaml:"description,omitempty"`
	Notes       string       `yaml:"notes,omitempty"`
	ContentType string       `yaml:"content_type,omitempty"` // article|listing|homepage|""
	Expected    CaseExpected `yaml:"expected"`
}

// CaseExpected scopes the rule-based scorer. All three lists are
// optional — a case with empty Expected still runs (and the scorer
// reports score=0 since there's nothing to hit), useful when you
// only care about the LLM-judge column.
type CaseExpected struct {
	// MustInclude tags weigh heaviest. Each hit adds +2 to the
	// raw score; max possible is 2 × len(MustInclude). A typical
	// case lists 1-2 here.
	MustInclude []string `yaml:"must_include,omitempty"`
	// NiceToHave tags weigh half. Each hit adds +1; misses do
	// nothing (unlike must_include misses which lower the
	// normalised score).
	NiceToHave []string `yaml:"nice_to_have,omitempty"`
	// MustNotInclude tags are tag-quality red flags — "技术" /
	// "互联网" / "人工智能" type filler. Each appearance subtracts 3
	// from the raw score and clamps the final normalised score down.
	MustNotInclude []string `yaml:"must_not_include,omitempty"`
	// Summary and Title describe deterministic substring contracts for the
	// generated content. Matching is case-insensitive and ignores whitespace so
	// Chinese typography differences do not make live model evals flaky.
	Summary TextExpected `yaml:"summary,omitempty"`
	Title   TextExpected `yaml:"title,omitempty"`
}

// TextExpected captures facts that must survive summarisation or title
// generation. MustIncludeAny is one alternative group: at least one listed
// phrase must appear when the group is non-empty.
type TextExpected struct {
	MustInclude    []string `yaml:"must_include,omitempty"`
	MustIncludeAny []string `yaml:"must_include_any,omitempty"`
	MustNotInclude []string `yaml:"must_not_include,omitempty"`
}

// LoadCaseFiles reads every *.yaml file under the supplied paths.
// A path may be a directory (recursed one level), a glob, or an
// exact file. Returns a flat slice across all matched files; case
// IDs must be globally unique and a duplicate triggers an error
// (so two files cannot silently shadow each other).
//
// At least one path must be supplied. Empty match list returns an
// error so an operator typo (`--case nosuchfile`) fails loudly
// instead of running zero cases and looking like a success.
func LoadCaseFiles(paths ...string) ([]Case, error) {
	if len(paths) == 0 {
		return nil, fmt.Errorf("eval: at least one case path required")
	}
	files, err := expandCasePaths(paths)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("eval: no case files matched %v", paths)
	}

	seen := make(map[string]string, 16)
	out := make([]Case, 0, 16)
	for _, file := range files {
		cases, err := readCaseFile(file)
		if err != nil {
			return nil, err
		}
		for _, c := range cases {
			if prev, ok := seen[c.ID]; ok {
				return nil, fmt.Errorf("eval: duplicate case id %q in %s (previously defined in %s)", c.ID, file, prev)
			}
			seen[c.ID] = file
			out = append(out, c)
		}
	}
	return out, nil
}

func expandCasePaths(paths []string) ([]string, error) {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		info, err := os.Stat(p)
		if err == nil && info.IsDir() {
			entries, err := os.ReadDir(p)
			if err != nil {
				return nil, fmt.Errorf("eval: read dir %s: %w", p, err)
			}
			for _, e := range entries {
				if e.IsDir() {
					continue
				}
				name := e.Name()
				if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
					continue
				}
				out = append(out, filepath.Join(p, name))
			}
			continue
		}
		// Glob (also handles plain file paths — Glob returns [p] for
		// an exact match).
		matches, err := filepath.Glob(p)
		if err != nil {
			return nil, fmt.Errorf("eval: glob %s: %w", p, err)
		}
		if len(matches) == 0 {
			// Plain non-existent path: surface as error so a typo
			// does not silently produce zero cases.
			if _, err := os.Stat(p); err != nil {
				return nil, fmt.Errorf("eval: case path not found: %s", p)
			}
		}
		out = append(out, matches...)
	}
	return out, nil
}

func readCaseFile(path string) ([]Case, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("eval: read %s: %w", path, err)
	}
	var file CaseFile
	if err := yaml.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("eval: parse %s: %w", path, err)
	}
	if file.Schema != CaseFileSchema {
		return nil, fmt.Errorf("eval: %s schema is %q, want %q (bump or rewrite)", path, file.Schema, CaseFileSchema)
	}
	for i, c := range file.Cases {
		if strings.TrimSpace(c.ID) == "" {
			return nil, fmt.Errorf("eval: %s case[%d] missing id", path, i)
		}
		if strings.TrimSpace(c.URL) == "" {
			return nil, fmt.Errorf("eval: %s case %q missing url", path, c.ID)
		}
	}
	return file.Cases, nil
}

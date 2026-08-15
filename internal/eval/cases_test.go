package eval

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTemp(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

const validCaseFile = `schema: webtag.eval/v1
cases:
  - id: a
    url: https://example.com/1
    expected:
      must_include: [foo]
      nice_to_have: [bar]
      must_not_include: [baz]
      summary:
        must_include: [decision]
        must_not_include: [guess]
      title:
        must_include_any: [paused, deferred]
        must_not_include: [cancelled]
  - id: b
    url: https://example.com/2
`

func TestLoadCaseFilesRoundTripsExpectedFields(t *testing.T) {
	t.Parallel()
	p := writeTemp(t, "cases.yaml", validCaseFile)
	cs, err := LoadCaseFiles(p)
	if err != nil {
		t.Fatalf("LoadCaseFiles: %v", err)
	}
	if len(cs) != 2 {
		t.Fatalf("len = %d, want 2", len(cs))
	}
	if cs[0].ID != "a" || cs[0].URL != "https://example.com/1" {
		t.Fatalf("first case = %+v", cs[0])
	}
	if got := cs[0].Expected.MustInclude; len(got) != 1 || got[0] != "foo" {
		t.Fatalf("must_include = %v", got)
	}
	if got := cs[0].Expected.Summary.MustInclude; len(got) != 1 || got[0] != "decision" {
		t.Fatalf("summary.must_include = %v", got)
	}
	if got := cs[0].Expected.Title.MustIncludeAny; len(got) != 2 || got[0] != "paused" {
		t.Fatalf("title.must_include_any = %v", got)
	}
}

func TestProductionURLCasesCaptureReviewedContentContracts(t *testing.T) {
	t.Parallel()

	path := filepath.Join("..", "..", "evals", "cases", "production_urls.yaml")
	cases, err := LoadCaseFiles(path)
	if err != nil {
		t.Fatalf("LoadCaseFiles: %v", err)
	}
	if len(cases) != 4 {
		t.Fatalf("production cases = %d, want 4", len(cases))
	}

	wantTypes := map[string]string{
		"production_github_daily_status": "article",
		"production_good_api_design":     "article",
		"production_ruanyf_weekly_402":   "listing",
		"production_go_error_syntax":     "article",
	}
	for _, c := range cases {
		if c.ContentType != wantTypes[c.ID] {
			t.Errorf("case %s content_type = %q, want %q", c.ID, c.ContentType, wantTypes[c.ID])
		}
		if len(c.Expected.MustInclude) < 2 {
			t.Errorf("case %s has weak required-tag contract: %v", c.ID, c.Expected.MustInclude)
		}
		if len(c.Expected.Summary.MustInclude) < 2 {
			t.Errorf("case %s has weak summary contract: %v", c.ID, c.Expected.Summary.MustInclude)
		}
	}
}

func TestLoadCaseFilesRejectsSchemaMismatch(t *testing.T) {
	t.Parallel()
	p := writeTemp(t, "cases.yaml", "schema: bogus\ncases: []\n")
	if _, err := LoadCaseFiles(p); err == nil || !strings.Contains(err.Error(), "schema") {
		t.Fatalf("err = %v, want schema mismatch", err)
	}
}

func TestLoadCaseFilesRejectsDuplicateIDs(t *testing.T) {
	t.Parallel()
	a := writeTemp(t, "a.yaml", "schema: webtag.eval/v1\ncases:\n  - id: dup\n    url: https://x/1\n")
	b := writeTemp(t, "b.yaml", "schema: webtag.eval/v1\ncases:\n  - id: dup\n    url: https://x/2\n")
	if _, err := LoadCaseFiles(a, b); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("err = %v, want duplicate error", err)
	}
}

func TestLoadCaseFilesRejectsMissingURL(t *testing.T) {
	t.Parallel()
	p := writeTemp(t, "broken.yaml", "schema: webtag.eval/v1\ncases:\n  - id: a\n")
	if _, err := LoadCaseFiles(p); err == nil || !strings.Contains(err.Error(), "missing url") {
		t.Fatalf("err = %v, want missing url error", err)
	}
}

func TestLoadCaseFilesRejectsTypoPath(t *testing.T) {
	t.Parallel()
	if _, err := LoadCaseFiles(filepath.Join(t.TempDir(), "no-such.yaml")); err == nil {
		t.Fatal("non-existent path should error, not silently produce zero cases")
	}
}

func TestLoadCaseFilesGlobAndDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for i, body := range []string{
		"schema: webtag.eval/v1\ncases:\n  - id: x\n    url: https://x/x\n",
		"schema: webtag.eval/v1\ncases:\n  - id: y\n    url: https://x/y\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, "f"+string(rune('1'+i))+".yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cs, err := LoadCaseFiles(dir)
	if err != nil {
		t.Fatalf("dir load: %v", err)
	}
	if len(cs) != 2 {
		t.Fatalf("dir load len = %d, want 2", len(cs))
	}
}

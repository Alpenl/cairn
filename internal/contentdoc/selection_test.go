package contentdoc

import (
	"encoding/json"
	"errors"
	"os"
	"testing"

	"webtag/internal/model"
)

type selectionProjectionFixture struct {
	Name       string              `json:"name"`
	Format     model.ContentFormat `json:"format"`
	Source     string              `json:"source"`
	Projection string              `json:"projection"`
	Selection  struct {
		Start int    `json:"start"`
		End   int    `json:"end"`
		Text  string `json:"text"`
	} `json:"selection"`
}

func loadSelectionProjectionFixtures(t *testing.T) []selectionProjectionFixture {
	t.Helper()

	data, err := os.ReadFile("../../test/fixtures/selection_projection.json")
	if err != nil {
		t.Fatalf("read shared selection projection fixture: %v", err)
	}
	var fixtures []selectionProjectionFixture
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatalf("decode shared selection projection fixture: %v", err)
	}
	if len(fixtures) == 0 {
		t.Fatal("shared selection projection fixture is empty")
	}
	return fixtures
}

func TestRenderedBlockProjectionMatchesReaderDOMContract(t *testing.T) {
	t.Parallel()

	for _, fixture := range loadSelectionProjectionFixtures(t) {
		fixture := fixture
		t.Run(fixture.Name, func(t *testing.T) {
			t.Parallel()

			got, err := RenderedBlockProjection(fixture.Format, fixture.Source)
			if err != nil {
				t.Fatalf("RenderedBlockProjection() error = %v", err)
			}
			if got != fixture.Projection {
				t.Fatalf("RenderedBlockProjection() = %q, want Reader DOM text %q", got, fixture.Projection)
			}
		})
	}
}

func TestValidateRenderedSelectionMatchesReaderContract(t *testing.T) {
	t.Parallel()

	for _, fixture := range loadSelectionProjectionFixtures(t) {
		fixture := fixture
		t.Run(fixture.Name, func(t *testing.T) {
			t.Parallel()

			err := ValidateRenderedSelection(
				fixture.Format,
				fixture.Source,
				fixture.Selection.Start,
				fixture.Selection.End,
				fixture.Selection.Text,
			)
			if err != nil {
				t.Fatalf("ValidateRenderedSelection() error = %v", err)
			}
		})
	}
}

func TestValidateRenderedSelectionRejectsNonCanonicalRanges(t *testing.T) {
	t.Parallel()

	const projection = "A😀B"
	tests := []struct {
		name     string
		start    int
		end      int
		selected string
	}{
		{name: "negative start", start: -1, end: 4, selected: "A😀B"},
		{name: "empty range", start: 1, end: 1, selected: ""},
		{name: "range beyond projection", start: 1, end: 5, selected: "😀B"},
		{name: "start splits surrogate pair", start: 2, end: 4, selected: "�B"},
		{name: "end splits surrogate pair", start: 0, end: 2, selected: "A�"},
		{name: "rune count end is not UTF-16 end", start: 1, end: 3, selected: "😀B"},
		{name: "selected text differs", start: 1, end: 4, selected: "😀C"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := ValidateRenderedSelection(model.ContentFormatPlain, projection, tt.start, tt.end, tt.selected)
			if !errors.Is(err, ErrSelectionMismatch) {
				t.Fatalf("ValidateRenderedSelection() error = %v, want ErrSelectionMismatch", err)
			}
		})
	}
}

func TestStrictGFMTableNormalizationOnlyTargetsPaddedHeaders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		source string
		want   string
	}{
		{
			name:   "delimiter-like lines inside a fenced code block stay source text",
			source: "```md\n| a | b |\n| --- | --- | --- |\n| x | y |\n```",
			want:   "| a | b |\n| --- | --- | --- |\n| x | y |\n",
		},
		{
			name:   "an explicit empty header cell remains a valid table",
			source: "| a | b |  |\n| --- | --- | --- |\n| x | y | z |",
			want:   "abxyz",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := RenderedBlockProjection(model.ContentFormatMarkdown, tt.source)
			if err != nil {
				t.Fatalf("RenderedBlockProjection() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("RenderedBlockProjection() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRenderedSourceBlockPersistenceHashUsesVersionedBlockDomain(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		blockKey   string
		projection string
		want       string
	}{
		{
			name:       "summary projection",
			blockKey:   "summary",
			projection: "hello world",
			want:       "f9fa260bd2039110c10e36a057949cc2e24104602debfb81911d93f948875bc4",
		},
		{
			name:       "same projection in another block",
			blockKey:   "dr",
			projection: "hello world",
			want:       "62e987cb9b3c27fdaec9be73b9bc2ddc4f015013636f53ab18e9a015c23a35f5",
		},
		{
			name:       "summary tail changes identity",
			blockKey:   "summary",
			projection: "hello world tail",
			want:       "fdecf973bec856d6d84dfc0ec5459ac484fd190ed950b1d8b8ce9baee028e871",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := RenderedSourceBlockPersistenceHash(tt.blockKey, tt.projection); got != tt.want {
				t.Fatalf("RenderedSourceBlockPersistenceHash() = %q, want %q", got, tt.want)
			}
		})
	}
}

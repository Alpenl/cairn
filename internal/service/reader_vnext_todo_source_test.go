package service

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"webtag/internal/model"
)

func TestReaderTodoSourceHrefUsesCanonicalReaderRoutes(t *testing.T) {
	t.Parallel()

	noteID := uuid.New().String()
	thoughtID := "thought one"
	cases := []struct {
		name string
		item model.ReaderTodo
		want string
	}{
		{name: "standalone", item: model.ReaderTodo{OriginKind: "standalone"}, want: ""},
		{name: "note", item: projectedTodoForSource("note", noteID, nil), want: "/?view=notes&note_id=" + noteID},
		{name: "inbox", item: projectedTodoForSource("inbox", "inbox/one", nil), want: "/?view=pending&inbox_id=inbox%2Fone"},
		{name: "thought", item: projectedTodoForSource("thought", thoughtID, nil), want: "/?tool=history&thought_view=live"},
		{
			name: "thought targets link",
			item: projectedTodoForSource("thought", thoughtID, json.RawMessage(`{"source_kind":"link","source_id":"link/one"}`)),
			want: "/?view=reading&link_id=link%2Fone",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := readerTodoSourceHref(tc.item)
			if tc.want == "" {
				if got != nil {
					t.Fatalf("readerTodoSourceHref() = %q, want nil", *got)
				}
				return
			}
			if got == nil || *got != tc.want {
				t.Fatalf("readerTodoSourceHref() = %v, want %q", got, tc.want)
			}
		})
	}
}

func projectedTodoForSource(kind, id string, ref json.RawMessage) model.ReaderTodo {
	return model.ReaderTodo{
		OriginKind:     kind,
		OriginHostKind: &kind,
		OriginHostID:   &id,
		OriginRef:      ref,
	}
}

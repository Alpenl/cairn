package service

import "testing"

func TestNormalizeTagLibraryKind(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		raw  string
		want string
		ok   bool
	}{
		{raw: "", want: "", ok: true},
		{raw: "reading", want: "reading", ok: true},
		{raw: " site ", want: "site", ok: true},
		{raw: " ALL ", want: "all", ok: true},
		{raw: "unknown", want: "", ok: false},
	} {
		got, ok := NormalizeTagLibraryKind(tc.raw)
		if got != tc.want || ok != tc.ok {
			t.Errorf("NormalizeTagLibraryKind(%q) = %q, %v; want %q, %v", tc.raw, got, ok, tc.want, tc.ok)
		}
	}
}

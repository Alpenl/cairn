package main

import (
	"bytes"
	"testing"

	"webtag/internal/buildinfo"
)

func TestVersionDoesNotRequireConfiguration(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	origV, origC, origB := buildinfo.Version, buildinfo.Commit, buildinfo.BuildTime
	t.Cleanup(func() {
		buildinfo.Version, buildinfo.Commit, buildinfo.BuildTime = origV, origC, origB
	})
	buildinfo.Version = "1.2.3"
	buildinfo.Commit = "0123456789abcdef"
	buildinfo.BuildTime = "2026-08-14T01:02:03Z"

	var stdout, stderr bytes.Buffer
	if err := execute([]string{"--version"}, &stdout, &stderr); err != nil {
		t.Fatalf("execute(--version) error = %v", err)
	}
	const want = "cairn 1.2.3\ncommit: 0123456789abcdef\nbuilt: 2026-08-14T01:02:03Z\n"
	if got := stdout.String(); got != want {
		t.Fatalf("execute(--version) stdout = %q, want %q", got, want)
	}
}

func TestWantsTodoProjectionRepairRequiresTheExplicitFlag(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name string
		args []string
		want bool
	}{
		{name: "no arguments"},
		{name: "unrelated argument", args: []string{"--version"}},
		{name: "explicit flag", args: []string{repairTodoProjectionsFlag}, want: true},
		{name: "explicit flag after another", args: []string{"--version", " " + repairTodoProjectionsFlag + " "}, want: true},
		{name: "near miss", args: []string{"--repair-reader-todo"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := wantsTodoProjectionRepair(tt.args); got != tt.want {
				t.Fatalf("wantsTodoProjectionRepair(%v) = %t, want %t", tt.args, got, tt.want)
			}
		})
	}
}

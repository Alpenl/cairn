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

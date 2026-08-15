package main

import (
	"fmt"
	"os"
	"strings"

	"webtag/internal/eval"
)

func loadCases(spec string) ([]eval.Case, error) {
	paths := splitCSV(spec)
	return eval.LoadCaseFiles(paths...)
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func loadRunJSON(path string) (eval.RunResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return eval.RunResult{}, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	return eval.LoadJSON(f)
}

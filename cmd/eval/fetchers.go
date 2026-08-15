package main

import (
	"fmt"

	"webtag/internal/eval"
)

func parseFetchers(csv string) ([]eval.FetcherName, error) {
	parts := splitCSV(csv)
	if len(parts) == 0 {
		return nil, fmt.Errorf("at least one --fetcher required")
	}
	out := make([]eval.FetcherName, len(parts))
	for i, p := range parts {
		out[i] = eval.FetcherName(p)
	}
	return out, nil
}

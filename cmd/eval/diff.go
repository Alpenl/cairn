package main

import (
	"flag"
	"fmt"
	"os"

	"webtag/internal/eval"
)

func diffCmd(args []string) int {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	threshold := fs.Float64("threshold", 0.01, "Hide cells whose rule and judge deltas are both below this absolute value.")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) != 2 {
		fmt.Fprintln(os.Stderr, "usage: eval diff [--threshold 0.01] <a.json> <b.json>")
		return 2
	}
	a, err := loadRunJSON(rest[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "eval:", err)
		return 1
	}
	b, err := loadRunJSON(rest[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "eval:", err)
		return 1
	}
	diffs := eval.DiffRuns(a, b, *threshold)
	if err := eval.RenderDiffMarkdown(os.Stdout, a, b, diffs); err != nil {
		fmt.Fprintln(os.Stderr, "eval:", err)
		return 1
	}
	return 0
}

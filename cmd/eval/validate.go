package main

import (
	"fmt"
	"os"

	"webtag/internal/eval"
)

func validateCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "eval validate: pass at least one case file")
		return 2
	}
	if _, err := eval.LoadCaseFiles(args...); err != nil {
		fmt.Fprintln(os.Stderr, "eval:", err)
		return 1
	}
	fmt.Println("ok")
	return 0
}

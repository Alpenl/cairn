package main

import (
	"flag"
	"fmt"
	"os"
)

func listCasesCmd(args []string) int {
	fs := flag.NewFlagSet("list-cases", flag.ContinueOnError)
	caseSpec := fs.String("case", "evals/cases", "Path or glob.")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cases, err := loadCases(*caseSpec)
	if err != nil {
		fmt.Fprintln(os.Stderr, "eval:", err)
		return 1
	}
	for _, c := range cases {
		fmt.Printf("%s\t%s\n", c.ID, c.URL)
	}
	return 0
}

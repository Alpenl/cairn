package main

import (
	"fmt"
	"os"
)

func usage() {
	fmt.Fprintln(os.Stderr, `eval — WebTag product evaluation harness

USAGE
  eval <subcommand> [flags]

SUBCOMMANDS
  run         Run the matrix and write a report.
  list-cases  Print the case IDs and URLs that would be loaded.
  validate    Parse a case file and report schema errors (exit 1 on failure).
  diff        Compare two run JSON files and print changed cells.

  eval run --help to see run-specific flags.`)
}

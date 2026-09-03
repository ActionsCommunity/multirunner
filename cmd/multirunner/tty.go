package main

import (
	"os"

	"github.com/mattn/go-isatty"
)

// isTerminal reports whether f is an interactive terminal, so prompts are only
// shown when a user can answer them. Unlike an os.ModeCharDevice check, this
// treats the null device (NUL, /dev/null) as not a terminal, so a CI or service
// job with stdin redirected from null never has prompts printed to it.
func isTerminal(f *os.File) bool {
	return isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd())
}

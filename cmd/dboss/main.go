package main

import (
	"os"

	"dboss/internal/cli"
	"dboss/internal/sandbox"
)

func main() {
	// The supervisor starts sandboxed commands through this binary; the launcher never returns.
	if len(os.Args) > 1 && os.Args[1] == sandbox.Arg {
		sandbox.Main(os.Args[2:])
	}
	os.Exit((cli.CLI{Out: os.Stdout, Err: os.Stderr}).Run(os.Args[1:]))
}

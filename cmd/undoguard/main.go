// Command undoguard is a safety belt for AI coding agents. It blocks
// destructive shell commands before they run, snapshots whatever an allowed
// command is about to touch, and restores it with one command.
package main

import (
	"os"

	"github.com/celaldrg/undoguard/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args, os.Stdin, os.Stdout, os.Stderr))
}

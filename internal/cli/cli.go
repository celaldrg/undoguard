// Package cli implements the undoguard command line.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/celaldrg/undoguard/internal/policy"
	"github.com/celaldrg/undoguard/internal/version"
)

// Exit codes. check uses exitAsk and exitDeny for its verdicts.
const (
	exitOK    = 0
	exitError = 1
	exitDeny  = 2
	exitAsk   = 3
)

// app carries the standard streams.
type app struct {
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
}

type command struct {
	run     func(a *app, args []string) int
	summary string
}

var commands map[string]command

func init() {
	commands = map[string]command{
		"check":   {cmdCheck, "show what the guard decides for a command line"},
		"hook":    {cmdHook, "run as an agent hook (claude, claude-post, codex, gemini, gemini-post)"},
		"exec":    {cmdExec, "check, snapshot and run one command (used by the shell wrapper)"},
		"run":     {cmdRun, "run an agent with every shell command it starts guarded"},
		"init":    {cmdInit, "install the hooks for Claude Code, Codex and Gemini CLI"},
		"log":     {cmdLog, "list snapshots"},
		"diff":    {cmdDiff, "show what changed since a snapshot"},
		"undo":    {cmdUndo, "restore the last snapshot, one by id, or a whole session"},
		"gc":      {cmdGC, "delete old snapshots and unused stored files"},
		"policy":  {cmdPolicy, "show the merged policy and where it came from"},
		"version": {cmdVersion, "print the version"},
	}
}

// Main runs the command line and returns the exit code.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	a := &app{stdin: stdin, stdout: stdout, stderr: stderr}
	if len(args) < 2 {
		a.usage(stderr)
		return exitError
	}
	name := args[1]
	switch name {
	case "-h", "--help", "help":
		a.usage(stdout)
		return exitOK
	case "-v", "--version":
		name = "version"
	}
	c, ok := commands[name]
	if !ok {
		fmt.Fprintf(stderr, "undoguard: unknown command %q\n\n", name)
		a.usage(stderr)
		return exitError
	}
	return c.run(a, args[2:])
}

func (a *app) usage(w io.Writer) {
	fmt.Fprintln(w, "undoguard blocks destructive shell commands from AI coding agents,")
	fmt.Fprintln(w, "snapshots what allowed commands touch, and undoes them on request.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage: undoguard <command> [options]")
	fmt.Fprintln(w)
	names := make([]string, 0, len(commands))
	for n := range commands {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(w, "  %-8s %s\n", n, commands[n].summary)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Run 'undoguard <command> -h' for the options of a command.")
}

// flags returns a flag set that prints its errors to stderr.
func (a *app) flags(name, args string) *flag.FlagSet {
	fs := flag.NewFlagSet("undoguard "+name, flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	fs.Usage = func() {
		fmt.Fprintf(a.stderr, "Usage: undoguard %s %s\n", name, args)
		var buf strings.Builder
		fs.SetOutput(&buf)
		fs.PrintDefaults()
		fs.SetOutput(a.stderr)
		if buf.Len() > 0 {
			fmt.Fprintf(a.stderr, "\nOptions:\n%s", buf.String())
		}
	}
	return fs
}

// parse parses flags and turns -h into a clean exit.
func parse(fs *flag.FlagSet, args []string) (int, bool) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK, false
		}
		return exitError, false
	}
	return 0, true
}

func (a *app) errorf(format string, args ...any) int {
	fmt.Fprintf(a.stderr, "undoguard: "+format+"\n", args...)
	return exitError
}

// workDir returns dir, or the current directory when dir is empty.
func workDir(dir string) (string, error) {
	if dir != "" {
		return filepath.Abs(dir)
	}
	return os.Getwd()
}

// loadPolicy loads the policy for a command run in dir and prints its
// warnings.
func (a *app) loadPolicy(dir string, quiet bool) (*policy.Policy, error) {
	p, err := policy.Load(dir)
	if err != nil {
		return nil, err
	}
	if !quiet {
		for _, w := range p.Warnings {
			fmt.Fprintf(a.stderr, "undoguard: warning: %s\n", w)
		}
	}
	return p, nil
}

func cmdVersion(a *app, args []string) int {
	fs := a.flags("version", "")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	fmt.Fprintln(a.stdout, "undoguard", version.String())
	return exitOK
}

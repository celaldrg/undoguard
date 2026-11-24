package cli

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/celaldrg/undoguard/internal/paths"
	"github.com/celaldrg/undoguard/internal/policy"
	"github.com/celaldrg/undoguard/internal/snapshot"
)

// exitBlocked is the exit code of a command that exec refused to run, as
// a shell reports a command it cannot execute.
const exitBlocked = 126

func cmdExec(a *app, args []string) int {
	fs := a.flags("exec", "[--shell PATH] [--agent NAME] [--session ID] -- <program or shell arguments>...")
	shell := fs.String("shell", "", "run this `shell` with the arguments, checking its -c script")
	agent := fs.String("agent", os.Getenv(envAgent), "agent `name` to record")
	session := fs.String("session", os.Getenv(envSession), "session `ID` to record")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	rest := fs.Args()
	if *shell == "" && len(rest) == 0 {
		fs.Usage()
		return exitError
	}
	if *shell == "" && len(rest) == 1 && strings.ContainsAny(rest[0], " \t\n;|&<>()$`") {
		// One argument that reads as a command line, such as
		// exec 'rm -rf build': run it with sh -c, as a shell would.
		if _, err := exec.LookPath(rest[0]); err != nil {
			*shell, rest = "/bin/sh", []string{"-c", rest[0]}
		}
	}
	prog, argv := *shell, rest
	if prog == "" {
		prog, argv = rest[0], rest[1:]
	}
	if os.Getenv(envActive) != "" {
		return a.runChild(prog, argv, nil)
	}
	dir, err := os.Getwd()
	if err != nil {
		return a.errorf("%v", err)
	}
	r := request{Dir: dir, Agent: *agent, Session: *session, AskBlocks: true}
	if *shell != "" {
		script, ok := shellScript(rest)
		if !ok {
			// An interactive shell or a script file: there is no command
			// line to check.
			return a.runChild(prog, argv, nil)
		}
		r.Command = script
	} else {
		r.Argv = rest
	}

	v := a.protect(r)
	switch v.Action {
	case policy.Deny:
		fmt.Fprintf(a.stderr, "undoguard: blocked: %s\n", v.Reason)
		return exitBlocked
	case policy.Ask:
		fmt.Fprintf(a.stderr, "undoguard: blocked, needs confirmation: %s\n", v.Reason)
		fmt.Fprintln(a.stderr, "undoguard: run it yourself outside the agent, or allow it in the undoguard policy")
		return exitBlocked
	}
	code := a.runChild(prog, argv, []string{envActive + "=1"})
	root := paths.DataDir()
	if v.Snapshot != "" {
		if _, err := snapshot.Finish(root, v.Snapshot); err != nil {
			fmt.Fprintf(a.stderr, "undoguard: warning: recording the result: %v\n", err)
		}
	}
	a.maybeGC(root)
	return code
}

// shellScript finds the script of `shell [options] -c script [args]`.
// It reports false for a shell that reads a script file or its input.
func shellScript(args []string) (string, bool) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--rcfile" || arg == "--init-file" || arg == "-o" || arg == "+o" || arg == "-O" || arg == "+O":
			i++
		case arg == "--" || arg == "-":
			return "", false
		case strings.HasPrefix(arg, "--"):
			// A long option without an argument, such as --login.
		case len(arg) > 1 && arg[0] == '-' && strings.ContainsRune(arg[1:], 'c'):
			if i+1 < len(args) {
				return args[i+1], true
			}
			return "", false
		case len(arg) > 1 && (arg[0] == '-' || arg[0] == '+'):
			// A cluster such as -e or -eu; -o in it takes the next word.
			if strings.ContainsAny(arg[1:], "oO") {
				i++
			}
		default:
			return "", false
		}
	}
	return "", false
}

// runChild runs prog with args, the standard streams and the environment
// plus extra, and returns its exit code. Ctrl-C reaches the child from the
// terminal, so the parent only waits for it to end; other termination
// signals are passed on.
func (a *app) runChild(prog string, args, extra []string) int {
	cmd := exec.Command(prog, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = a.stdin, a.stdout, a.stderr
	cmd.Env = append(os.Environ(), extra...)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(a.stderr, "undoguard: %v\n", err)
		return 127
	}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case s := <-sigs:
				if s != os.Interrupt {
					_ = cmd.Process.Signal(s)
				}
			case <-done:
				return
			}
		}
	}()
	err := cmd.Wait()
	signal.Stop(sigs)
	close(done)
	return exitStatus(err)
}

// exitStatus turns the result of a child into an exit code, the way a
// shell does.
func exitStatus(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return 1
	}
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ee.ExitCode()
}

// shimShells are the shells that run puts in front of the real ones.
var shimShells = []string{"sh", "bash", "zsh", "dash", "ksh"}

func cmdRun(a *app, args []string) int {
	fs := a.flags("run", "[--agent NAME] -- <agent> [arguments]...")
	agent := fs.String("agent", "", "agent `name` to record (default: the program name)")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return exitError
	}
	self, err := os.Executable()
	if err != nil {
		return a.errorf("finding the undoguard program: %v", err)
	}
	dir, err := os.MkdirTemp("", "undoguard-run-")
	if err != nil {
		return a.errorf("%v", err)
	}
	defer os.RemoveAll(dir)

	installed := map[string]bool{}
	for _, name := range shimShells {
		target, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		if target, err = filepath.Abs(target); err != nil {
			continue
		}
		if err := writeShim(filepath.Join(dir, name), self, target); err != nil {
			return a.errorf("%v", err)
		}
		installed[name] = true
	}
	if len(installed) == 0 {
		return a.errorf("no shell found on PATH")
	}

	session := os.Getenv(envSession)
	if session == "" {
		if session, err = newSession(); err != nil {
			return a.errorf("%v", err)
		}
	}
	name := *agent
	if name == "" {
		name = filepath.Base(rest[0])
	}
	env := []string{
		"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"),
		envSession + "=" + session,
		envAgent + "=" + name,
		envActive + "=",
	}
	shellName := filepath.Base(os.Getenv("SHELL"))
	if !installed[shellName] {
		shellName = "sh"
	}
	if installed[shellName] {
		env = append(env, "SHELL="+filepath.Join(dir, shellName))
	}
	fmt.Fprintf(a.stderr, "undoguard: guarding the shell commands of %s, session %s\n", name, session)
	fmt.Fprintf(a.stderr, "undoguard: 'undoguard undo --session-id %s' undoes them all\n", session)
	return a.runChild(rest[0], rest[1:], env)
}

// writeShim writes a script that sends a shell's arguments through
// undoguard exec to the real shell.
func writeShim(path, self, shell string) error {
	script := "#!/bin/sh\n" +
		"# Written by undoguard run; removed when the agent exits.\n" +
		"exec " + shQuote(self) + " exec --shell " + shQuote(shell) + " -- \"$@\"\n"
	return os.WriteFile(path, []byte(script), 0o755)
}

// shQuote quotes s for a POSIX shell.
func shQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_./-+:=@%") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// newSession returns a random session id.
func newSession() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "run-" + hex.EncodeToString(b), nil
}

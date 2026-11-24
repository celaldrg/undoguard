package cli

import (
	"fmt"
	"os"
	"time"

	"github.com/celaldrg/undoguard/internal/guard"
	"github.com/celaldrg/undoguard/internal/hooks"
	"github.com/celaldrg/undoguard/internal/journal"
	"github.com/celaldrg/undoguard/internal/paths"
	"github.com/celaldrg/undoguard/internal/policy"
	"github.com/celaldrg/undoguard/internal/snapshot"
)

// Environment variables that tie the commands of one agent run together.
const (
	envSession = "UNDOGUARD_SESSION"
	envAgent   = "UNDOGUARD_AGENT"
	// envActive is set for the commands that run below a checked command
	// line, whose nested shells were part of what was checked.
	envActive = "UNDOGUARD_ACTIVE"
)

// pendingAge bounds how long a command may run between the hooks before
// and after it.
const pendingAge = 24 * time.Hour

// request is one command line to guard.
type request struct {
	Command string
	// Argv is set instead of Command for one program and its arguments.
	Argv []string
	// Edits is set instead for the files an agent's file tool changes;
	// Command then describes the call.
	Edits   []guard.Edit
	Dir     string
	Agent   string
	Session string
	// AskBlocks is set when nobody can answer an ask, so that no
	// snapshot is taken for a command that will not run.
	AskBlocks bool
}

// verdict is the guard's answer once the snapshot is taken.
type verdict struct {
	Action policy.Action
	Reason string
	// Snapshot is the id of the snapshot taken, if any.
	Snapshot string
}

// protect decides on a command line, snapshots what it touches when it
// may run, and records the decision in the journal.
//
// When the snapshot cannot be taken in full, an allow becomes an ask: the
// command may still be wanted, but undo would not cover all of it.
func (a *app) protect(r request) verdict {
	root := paths.DataDir()
	p, err := a.loadPolicy(r.Dir, true)
	if err != nil {
		v := verdict{Action: policy.Ask, Reason: "the undoguard policy could not be loaded: " + err.Error()}
		a.record(root, r, r.Command, v)
		return v
	}
	o := guard.Options{Dir: r.Dir, Policy: p}
	var d *guard.Decision
	switch {
	case r.Edits != nil:
		d = guard.CheckEdits(r.Command, r.Edits, o)
	case r.Argv != nil:
		d = guard.CheckArgv(r.Argv, o)
	default:
		d = guard.Check(r.Command, o)
	}
	v := verdict{Action: d.Action, Reason: d.Reason()}
	wanted := d.Action == policy.Allow || (d.Action == policy.Ask && !r.AskBlocks)
	if wanted && len(d.Snapshot) > 0 {
		roots := make([]snapshot.Root, 0, len(d.Snapshot))
		for _, it := range d.Snapshot {
			roots = append(roots, snapshot.Root{
				Path:      it.Path,
				Recursive: it.Recursive,
				Follow:    it.Follow,
				Git:       string(it.Git),
				Repo:      it.Repo,
			})
		}
		m, err := snapshot.Take(roots, snapshot.Options{
			Root:    root,
			Limits:  p.Limits,
			Session: r.Session,
			Agent:   r.Agent,
			Command: d.Command,
			Dir:     r.Dir,
		})
		if m != nil {
			v.Snapshot = m.ID
		}
		if err != nil {
			v.Action = policy.Ask
			v.Reason = "undo would not cover all of it, since the snapshot failed (" + err.Error() + "); " + v.Reason
		}
	}
	if d.Destructive() || v.Action != policy.Allow {
		a.record(root, r, d.Command, v)
	}
	return v
}

// record appends a decision to the journal.
func (a *app) record(root string, r request, command string, v verdict) {
	event := journal.EventDeny
	switch v.Action {
	case policy.Allow:
		event = journal.EventAllow
	case policy.Ask:
		event = journal.EventAsk
	}
	err := journal.Append(root, journal.Entry{
		Event:    event,
		Agent:    r.Agent,
		Session:  r.Session,
		Command:  command,
		Dir:      r.Dir,
		Reason:   v.Reason,
		Snapshot: v.Snapshot,
	})
	if err != nil {
		fmt.Fprintf(a.stderr, "undoguard: warning: writing the journal: %v\n", err)
	}
}

// maybeGC cleans up once a day, without ever waiting for a lock.
func (a *app) maybeGC(root string) {
	if _, err := snapshot.MaybeGC(root, a.limits(), time.Now()); err != nil {
		fmt.Fprintf(a.stderr, "undoguard: warning: cleaning up: %v\n", err)
	}
}

func cmdHook(a *app, args []string) int {
	fs := a.flags("hook", "<claude | claude-post | codex | codex-post | gemini | gemini-post>")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return exitError
	}
	kind, ok := hooks.Kinds[fs.Arg(0)]
	if !ok {
		return a.errorf("unknown hook %q", fs.Arg(0))
	}
	// An error exit lets the tool call go on; the agents show the message.
	// Failing open on a broken call keeps a bug in undoguard from locking
	// the agent out of its shell.
	ev, err := hooks.Parse(kind.Agent, a.stdin)
	if err != nil {
		return a.errorf("%v", err)
	}
	if ev.Command == "" {
		return exitOK
	}
	if ev.Session == "" {
		ev.Session = os.Getenv(envSession)
	}
	dir, err := workDir(ev.Dir)
	if err != nil {
		return a.errorf("%v", err)
	}
	root := paths.DataDir()
	now := time.Now()

	if kind.Post {
		m, err := snapshot.Pending(root, ev.Session, ev.Command, pendingAge, now)
		if err == nil && m != nil {
			_, err = snapshot.Finish(root, m.ID)
		}
		if err != nil {
			fmt.Fprintf(a.stderr, "undoguard: warning: recording the result: %v\n", err)
		}
		a.maybeGC(root)
		return exitOK
	}

	if kind.Agent == hooks.Codex && ev.Session != "" {
		// Older Codex versions have no hook after a tool, and Codex runs
		// one tool at a time: when the next one arrives, the earlier ones
		// are done. With codex-post installed this finds nothing.
		ids, err := snapshot.Unfinished(root, ev.Session, pendingAge, now)
		for _, id := range ids {
			if err == nil {
				_, err = snapshot.Finish(root, id)
			}
		}
		if err != nil {
			fmt.Fprintf(a.stderr, "undoguard: warning: recording earlier results: %v\n", err)
		}
	}

	v := a.protect(request{
		Command: ev.Command, Edits: ev.Edits, Dir: dir, Agent: kind.Agent, Session: ev.Session,
		// Only Claude Code can put a question to the user from a hook;
		// for the others an ask blocks, so there is nothing to snapshot.
		AskBlocks: kind.Agent != hooks.Claude,
	})
	if err := hooks.Write(a.stdout, kind.Agent, v.Action, v.Reason); err != nil {
		return a.errorf("%v", err)
	}
	return exitOK
}

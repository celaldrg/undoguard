package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/celaldrg/undoguard/internal/journal"
	"github.com/celaldrg/undoguard/internal/paths"
	"github.com/celaldrg/undoguard/internal/policy"
	"github.com/celaldrg/undoguard/internal/snapshot"
	"github.com/celaldrg/undoguard/internal/store"
	"github.com/celaldrg/undoguard/internal/textdiff"
)

// timeLayout is how times are shown, in local time.
const timeLayout = "2006-01-02 15:04:05"

// short puts s on one line and cuts it to n characters.
func short(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

type logJSON struct {
	ID      string    `json:"id"`
	Created time.Time `json:"created"`
	Status  string    `json:"status"`
	Agent   string    `json:"agent,omitempty"`
	Session string    `json:"session,omitempty"`
	Command string    `json:"command"`
	Dir     string    `json:"dir,omitempty"`
	Paths   int       `json:"paths"`
	Files   int       `json:"files"`
	Bytes   int64     `json:"bytes"`
	Undoes  string    `json:"undoes,omitempty"`
}

func cmdLog(a *app, args []string) int {
	fs := a.flags("log", "[--all] [--json] [-n N] [--session ID]")
	all := fs.Bool("all", false, "list every guard decision from the journal instead of the snapshots")
	asJSON := fs.Bool("json", false, "print JSON")
	limit := fs.Int("n", 20, "show at most `N` entries, 0 for all")
	session := fs.String("session", "", "only show the session with this `ID`")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return exitError
	}
	root := paths.DataDir()
	if *all {
		return a.logJournal(root, *asJSON, *limit, *session)
	}
	list, err := snapshot.List(root)
	if err != nil {
		return a.errorf("%v", err)
	}
	var shown []*snapshot.Manifest
	for _, m := range list {
		if *session == "" || m.Session == *session {
			shown = append(shown, m)
		}
	}
	if *limit > 0 && len(shown) > *limit {
		shown = shown[:*limit]
	}
	if *asJSON {
		out := []logJSON{}
		for _, m := range shown {
			out = append(out, logJSON{
				ID:      m.ID,
				Created: m.Created,
				Status:  m.Status(),
				Agent:   m.Agent,
				Session: m.Session,
				Command: journal.Redact(m.Command),
				Dir:     m.Dir,
				Paths:   len(m.Entries),
				Files:   m.Files(),
				Bytes:   m.Bytes,
				Undoes:  m.Undoes,
			})
		}
		return a.printJSON(out)
	}
	if len(shown) == 0 {
		fmt.Fprintln(a.stdout, "No snapshots yet.")
		return exitOK
	}
	tw := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTAKEN\tAGENT\tSTATUS\tPATHS\tSTORED\tCOMMAND")
	for _, m := range shown {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n", m.ID, m.Created.Local().Format(timeLayout), dash(m.Agent),
			m.Status(), len(m.Entries), policy.FormatSize(m.Bytes), short(journal.Redact(m.Command), 60))
	}
	tw.Flush()
	if len(list) > len(shown) && *session == "" {
		fmt.Fprintf(a.stdout, "\n%d older %s not shown; -n 0 lists all.\n", len(list)-len(shown),
			plural(len(list)-len(shown), "snapshot", "snapshots"))
	}
	return exitOK
}

func (a *app) logJournal(root string, asJSON bool, limit int, session string) int {
	entries, err := journal.Read(root)
	if err != nil {
		return a.errorf("%v", err)
	}
	var shown []journal.Entry
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if session != "" && e.Session != session {
			continue
		}
		shown = append(shown, e)
		if limit > 0 && len(shown) == limit {
			break
		}
	}
	if asJSON {
		if shown == nil {
			shown = []journal.Entry{}
		}
		return a.printJSON(shown)
	}
	if len(shown) == 0 {
		fmt.Fprintln(a.stdout, "The journal is empty.")
		return exitOK
	}
	tw := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TIME\tEVENT\tAGENT\tSNAPSHOT\tCOMMAND\tREASON")
	for _, e := range shown {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", e.Time.Local().Format(timeLayout), e.Event, dash(e.Agent),
			dash(e.Snapshot), short(e.Command, 50), short(e.Reason, 70))
	}
	tw.Flush()
	return exitOK
}

func (a *app) printJSON(v any) int {
	enc := json.NewEncoder(a.stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return a.errorf("%v", err)
	}
	return exitOK
}

// maxDiffBytes bounds the files diff reads.
const maxDiffBytes = 1 << 20

func cmdDiff(a *app, args []string) int {
	fs := a.flags("diff", "[--stat] [<id>]")
	stat := fs.Bool("stat", false, "list the changed paths without their content")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return exitError
	}
	root := paths.DataDir()
	// Without an id, diff shows the snapshot that undo would restore.
	var m *snapshot.Manifest
	var err error
	if fs.NArg() == 1 {
		m, err = snapshot.Find(root, fs.Arg(0))
	} else {
		m, err = snapshot.Latest(root, "")
	}
	if err != nil {
		return a.errorf("%v", err)
	}
	w := a.stdout
	fmt.Fprintf(w, "Snapshot %s, taken %s (%s)\n", m.ID, m.Created.Local().Format(timeLayout), m.Status())
	fmt.Fprintf(w, "Command:  %s\n", journal.Redact(m.Command))
	plan := snapshot.PlanRestore(m)
	if len(plan.Changes) == 0 {
		fmt.Fprintf(w, "\nAll %d %s match the snapshot.\n", plan.Unchanged, plural(plan.Unchanged, "path", "paths"))
		return exitOK
	}
	fmt.Fprintf(w, "\n%d %s differ from the snapshot, %d match it.\n", len(plan.Changes),
		plural(len(plan.Changes), "path", "paths"), plan.Unchanged)
	var st *store.Store
	for _, c := range plan.Changes {
		fmt.Fprintf(w, "\n%-10s %s\n", describe(c), paths.Display(c.Path))
		if c.Reason != "" {
			fmt.Fprintf(w, "           cannot be restored: %s\n", c.Reason)
		}
		if c.Conflict != "" {
			fmt.Fprintf(w, "           note: %s\n", c.Conflict)
		}
		if *stat || !textual(c) {
			continue
		}
		if st == nil {
			if st, err = store.Open(root); err != nil {
				return a.errorf("%v", err)
			}
		}
		before, err := readObject(st, c.Want.Hash)
		if err != nil {
			fmt.Fprintf(w, "           (snapshot copy unreadable: %v)\n", err)
			continue
		}
		after, err := readCapped(c.Path, c.Now.Kind == snapshot.KindFile)
		if err != nil {
			fmt.Fprintf(w, "           (%v)\n", err)
			continue
		}
		label := paths.Display(c.Path)
		io.WriteString(w, textdiff.Unified(label+" (snapshot)", label+" (now)", before, after, 3))
	}
	return exitOK
}

// textual reports whether a change has file content on both sides worth
// a line diff.
func textual(c snapshot.Change) bool {
	fileOrNone := func(s snapshot.State) bool {
		return s.Kind == snapshot.KindFile || s.Kind == snapshot.KindAbsent
	}
	if !fileOrNone(c.Want) || !fileOrNone(c.Now) {
		return false
	}
	if c.Want.Kind == snapshot.KindFile && c.Want.Hash == "" {
		return false
	}
	return c.Kind == snapshot.ChangeRestore || c.Kind == snapshot.ChangeRemove
}

func describe(c snapshot.Change) string {
	switch c.Kind {
	case snapshot.ChangeRemove:
		return "created"
	case snapshot.ChangeRecreate:
		if c.Now.Kind == snapshot.KindAbsent {
			return "deleted"
		}
		return "replaced"
	case snapshot.ChangeMetadata:
		return "mode"
	case snapshot.ChangeRestore:
		switch {
		case c.Now.Kind == snapshot.KindAbsent:
			return "deleted"
		case c.Now.Kind != c.Want.Kind:
			return "replaced"
		}
		return "modified"
	}
	return "changed"
}

func readObject(st *store.Store, hash string) ([]byte, error) {
	if hash == "" {
		return nil, nil
	}
	f, err := st.Open(hash)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readLimited(f)
}

// readCapped reads a file that undo would replace or remove, if it is a
// regular file.
func readCapped(p string, isFile bool) ([]byte, error) {
	if !isFile {
		return nil, nil
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readLimited(f)
}

func readLimited(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxDiffBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxDiffBytes {
		return nil, fmt.Errorf("larger than %s, not shown", policy.FormatSize(maxDiffBytes))
	}
	return b, nil
}

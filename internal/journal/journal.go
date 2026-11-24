// Package journal appends one JSON line per guard decision and undo to a
// log in the data directory, so that `undoguard log --all` can show what
// was blocked or questioned as well as what was snapshotted.
//
// Command lines can hold secrets. Before a line is written, values that
// look like passwords, tokens or credentials in URLs are masked. The
// masking is a best effort, not a guarantee.
package journal

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/celaldrg/undoguard/internal/flock"
)

// Events written to the journal.
const (
	EventAllow    = "allow"
	EventAsk      = "ask"
	EventDeny     = "deny"
	EventSnapshot = "snapshot"
	EventUndo     = "undo"
	EventError    = "error"
)

// maxSize is the size at which the journal is rotated. One older
// generation is kept.
const maxSize = 10 << 20

// Entry is one line of the journal.
type Entry struct {
	Time    time.Time `json:"time"`
	Event   string    `json:"event"`
	Agent   string    `json:"agent,omitempty"`
	Session string    `json:"session,omitempty"`
	Command string    `json:"command,omitempty"`
	Dir     string    `json:"dir,omitempty"`
	Reason  string    `json:"reason,omitempty"`
	// Snapshot is the id of the snapshot taken or undone.
	Snapshot string `json:"snapshot,omitempty"`
}

// Path is the journal file below the data directory root.
func Path(root string) string {
	return filepath.Join(root, "journal.jsonl")
}

// Append writes e to the journal, masking secrets in its text fields.
func Append(root string, e Entry) error {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	e.Time = e.Time.UTC().Truncate(time.Millisecond)
	e.Command = Redact(e.Command)
	e.Reason = Redact(e.Reason)
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	lock, err := flock.Exclusive(filepath.Join(root, "journal.lock"))
	if err != nil {
		return err
	}
	defer lock.Release()

	p := Path(root)
	if info, err := os.Stat(p); err == nil && info.Size()+int64(len(line)) > maxSize {
		if err := os.Rename(p, p+".1"); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Read returns the journal, oldest first. Lines that do not parse are
// skipped.
func Read(root string) ([]Entry, error) {
	var out []Entry
	for _, p := range []string{Path(root) + ".1", Path(root)} {
		f, err := os.Open(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		for sc.Scan() {
			var e Entry
			if json.Unmarshal(sc.Bytes(), &e) == nil {
				out = append(out, e)
			}
		}
		err = sc.Err()
		f.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

var redactions = []struct {
	re   *regexp.Regexp
	with string
}{
	// user:password@ in URLs.
	{regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://[^/\s:@]*:)[^@\s/]+@`), "${1}***@"},
	// VAR=value where the name says it holds a secret.
	{regexp.MustCompile(`(?i)\b([A-Z0-9_]*(?:PASSWORD|PASSWD|PASS|TOKEN|SECRET|API_?KEY|ACCESS_?KEY|CREDENTIALS?)[A-Z0-9_]*=)("[^"]*"|'[^']*'|\S+)`), "${1}***"},
	// --password value, --token=value and the like.
	{regexp.MustCompile(`(?i)(--?(?:password|passwd|pass|token|secret|api-?key|access-?key)(?:=|\s+))("[^"]*"|'[^']*'|\S+)`), "${1}***"},
	// mysql -psecret.
	{regexp.MustCompile(`(\bmysql(?:dump|admin)?\b[^|;&\n]*?\s-p)([^\s'"]\S*|"[^"]*"|'[^']*')`), "${1}***"},
	// Authorization headers.
	{regexp.MustCompile(`(?i)(authorization:\s*(?:bearer|basic|token)\s+)[^\s'"]+`), "${1}***"},
}

// Redact masks values that look like secrets.
func Redact(s string) string {
	for _, r := range redactions {
		s = r.re.ReplaceAllString(s, r.with)
	}
	return s
}

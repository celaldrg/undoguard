package journal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	cases := []struct{ in, want string }{
		{"rm -rf build", "rm -rf build"},
		{"curl https://user:hunter2@example.com/x", "curl https://user:***@example.com/x"},
		{"API_TOKEN=abc123 ./deploy", "API_TOKEN=*** ./deploy"},
		{"export DB_PASSWORD='p w'", "export DB_PASSWORD=***"},
		{"tool --token abc --verbose", "tool --token *** --verbose"},
		{"tool --password=abc", "tool --password=***"},
		{"mysql -u root -psecret shop", "mysql -u root -p*** shop"},
		{"mysql -u root -p shop", "mysql -u root -p shop"},
		{`curl -H "Authorization: Bearer abc.def" https://example.com`, `curl -H "Authorization: Bearer ***" https://example.com`},
	}
	for _, c := range cases {
		if got := Redact(c.in); got != c.want {
			t.Errorf("Redact(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestAppendAndRead(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data")
	entries := []Entry{
		{Event: EventDeny, Agent: "claude", Command: "rm -rf ~", Reason: "home"},
		{Event: EventAllow, Command: "API_KEY=abc make", Snapshot: "s1"},
	}
	for _, e := range entries {
		if err := Append(root, e); err != nil {
			t.Fatal(err)
		}
	}
	got, err := Read(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("read %d entries, want 2", len(got))
	}
	if got[0].Event != EventDeny || got[0].Agent != "claude" || got[0].Time.IsZero() {
		t.Errorf("first entry = %+v", got[0])
	}
	if got[1].Command != "API_KEY=*** make" || got[1].Snapshot != "s1" {
		t.Errorf("second entry = %+v", got[1])
	}
	info, err := os.Stat(Path(root))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("journal mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestReadSkipsBrokenLines(t *testing.T) {
	root := t.TempDir()
	content := `{"event":"deny","command":"a"}` + "\nnot json\n" + `{"event":"allow","command":"b"}` + "\n"
	if err := os.WriteFile(Path(root), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Read(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Command != "a" || got[1].Command != "b" {
		t.Errorf("got %+v", got)
	}
}

func TestReadMissing(t *testing.T) {
	got, err := Read(filepath.Join(t.TempDir(), "none"))
	if err != nil || len(got) != 0 {
		t.Errorf("got %v, %v", got, err)
	}
}

func TestReadIncludesRotated(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(Path(root)+".1", []byte(`{"event":"deny","command":"old"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Append(root, Entry{Event: EventAllow, Command: "new"}); err != nil {
		t.Fatal(err)
	}
	got, err := Read(root)
	if err != nil {
		t.Fatal(err)
	}
	var cmds []string
	for _, e := range got {
		cmds = append(cmds, e.Command)
	}
	if strings.Join(cmds, ",") != "old,new" {
		t.Errorf("order = %v", cmds)
	}
}

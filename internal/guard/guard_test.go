package guard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/celaldrg/undoguard/internal/policy"
)

// world is a throwaway home directory with a project inside it.
type world struct {
	base, home, proj, data, conf string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	base := t.TempDir()
	if r, err := filepath.EvalSymlinks(base); err == nil {
		base = r
	}
	w := &world{
		base: base,
		home: filepath.Join(base, "home"),
		data: filepath.Join(base, "data"),
		conf: filepath.Join(base, "conf"),
	}
	w.proj = filepath.Join(w.home, "project")
	t.Setenv("HOME", w.home)
	t.Setenv("UNDOGUARD_HOME", w.data)
	t.Setenv("UNDOGUARD_CONFIG_DIR", w.conf)
	for _, k := range []string{"UNDOGUARD_PROJECT", "CLAUDE_PROJECT_DIR", "GEMINI_PROJECT_DIR", "XDG_DATA_HOME", "XDG_CONFIG_HOME"} {
		t.Setenv(k, "")
	}
	for _, d := range []string{
		w.conf,
		filepath.Join(w.proj, ".git"),
		filepath.Join(w.proj, "build", "out"),
		filepath.Join(w.proj, "src"),
		filepath.Join(w.home, "important"),
		filepath.Join(w.home, ".ssh"),
		filepath.Join(base, "scratch"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{
		filepath.Join(w.proj, "README.md"),
		filepath.Join(w.proj, "src", "main.go"),
		filepath.Join(w.proj, "build", "out", "app"),
		filepath.Join(w.home, "important", "thesis.txt"),
		filepath.Join(w.home, ".ssh", "id_test"),
		filepath.Join(w.home, "notes.txt"),
		filepath.Join(base, "scratch", "junk.log"),
		filepath.Join(base, "elsewhere.txt"),
	} {
		if err := os.WriteFile(f, []byte("data\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return w
}

func (w *world) writePolicy(t *testing.T, user, project string) {
	t.Helper()
	if user != "" {
		if err := os.WriteFile(filepath.Join(w.conf, "policy.toml"), []byte(user), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if project != "" {
		if err := os.WriteFile(filepath.Join(w.proj, policy.ProjectFileName), []byte(project), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func (w *world) check(t *testing.T, cmd string) *Decision {
	t.Helper()
	p, err := policy.Load(w.proj)
	if err != nil {
		t.Fatalf("loading the policy: %v", err)
	}
	return Check(cmd, Options{Dir: w.proj, Policy: p})
}

func describe(d *Decision) string {
	var b strings.Builder
	b.WriteString(string(d.Action) + ": " + d.Reason())
	for _, a := range d.Assessments {
		b.WriteString("\n    " + string(a.Action) + " " + a.Rule + " " + a.Subject() + " (" + string(a.Class) + ")")
	}
	return b.String()
}

func TestProjectWorkIsAllowedAndSnapshotted(t *testing.T) {
	w := newWorld(t)
	d := w.check(t, "rm -rf build")
	if d.Action != policy.Allow {
		t.Fatalf("got %s", describe(d))
	}
	if !d.Destructive() {
		t.Error("rm -rf should count as destructive")
	}
	want := filepath.Join(w.proj, "build")
	found := false
	for _, s := range d.Snapshot {
		if s.Path == want && s.Recursive {
			found = true
		}
	}
	if !found {
		t.Errorf("snapshot %+v does not hold %s recursively", d.Snapshot, want)
	}
}

func TestHarmlessCommands(t *testing.T) {
	w := newWorld(t)
	for _, cmd := range []string{
		"ls -la",
		"git status",
		"cat README.md | grep data",
		"echo hello",
		"go test ./...",
		"cat README.md > /dev/null",
		"rm --help",
	} {
		d := w.check(t, cmd)
		if d.Action != policy.Allow || d.Destructive() {
			t.Errorf("%q: %s", cmd, describe(d))
		}
		if len(d.Snapshot) != 0 {
			t.Errorf("%q: unexpected snapshot %+v", cmd, d.Snapshot)
		}
	}
}

func TestMissingTargetIsAllowedButSnapshotted(t *testing.T) {
	w := newWorld(t)
	d := w.check(t, "echo hi > new.txt")
	if d.Action != policy.Allow {
		t.Fatalf("got %s", describe(d))
	}
	if len(d.Snapshot) != 1 || d.Snapshot[0].Path != filepath.Join(w.proj, "new.txt") {
		t.Errorf("snapshot = %+v", d.Snapshot)
	}
}

func TestDenyClearsSnapshot(t *testing.T) {
	w := newWorld(t)
	d := w.check(t, "rm -rf build ~/important")
	if d.Action != policy.Deny {
		t.Fatalf("got %s", describe(d))
	}
	if len(d.Snapshot) != 0 {
		t.Errorf("a denied command line should not be snapshotted: %+v", d.Snapshot)
	}
}

func TestReasonNamesTheTarget(t *testing.T) {
	w := newWorld(t)
	d := w.check(t, "rm -rf ~/project/../important")
	if d.Action != policy.Deny {
		t.Fatalf("got %s", describe(d))
	}
	if !strings.Contains(d.Reason(), "~/important") {
		t.Errorf("reason %q should name ~/important", d.Reason())
	}
	if d.Deciding == nil || d.Deciding.Class != policy.ClassHome {
		t.Errorf("deciding assessment = %+v", d.Deciding)
	}
}

// bypass is one attempt to get a destructive command past the guard.
type bypass struct {
	cmd  string
	want policy.Action
	// atLeast accepts any stricter action too.
	atLeast bool
}

func runTable(t *testing.T, w *world, cases []bypass) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.cmd, func(t *testing.T) {
			d := w.check(t, c.cmd)
			ok := d.Action == c.want
			if c.atLeast {
				ok = d.Action.Rank() >= c.want.Rank()
			}
			if !ok {
				t.Errorf("want %s, got %s", c.want, describe(d))
			}
		})
	}
}

// TestBypassTable lists the ways an agent could word a destructive command
// line. None of them may be allowed.
func TestBypassTable(t *testing.T) {
	w := newWorld(t)
	deny, ask := policy.Deny, policy.Ask
	runTable(t, w, []bypass{
		// The home directory and what is in it.
		{cmd: "rm -rf ~", want: deny},
		{cmd: "rm -rf ~/", want: deny},
		{cmd: "rm -rf $HOME", want: deny},
		{cmd: `rm -rf "$HOME"`, want: deny},
		{cmd: "rm -rf ${HOME}/important", want: deny},
		{cmd: "rm -rf ~/project/../important", want: deny},
		{cmd: "rm -rf ../important", want: deny},
		{cmd: "rm -r -f ../important", want: deny},
		{cmd: "rm --recursive --force ../important", want: deny},
		{cmd: "rm -rf -- ../important", want: deny},
		{cmd: "rm ~/notes.txt", want: deny},
		{cmd: "rm -rf ~/*", want: deny},
		{cmd: "rm -rf ~/.ssh", want: deny},
		{cmd: "rm ~/.ssh/id_test", want: deny},
		{cmd: "rm -rf ~/.config", want: deny},

		// System paths.
		{cmd: "rm -rf /", want: deny},
		{cmd: "rm -rf /*", want: deny},
		{cmd: "rm -rf /etc", want: deny},
		{cmd: "rm -rf /usr/local", want: deny},
		{cmd: "chmod -R 777 /etc", want: deny},
		{cmd: "chown -R nobody ~", want: deny},

		// Wrappers and prefixes.
		{cmd: "sudo rm -rf ~/important", want: deny},
		{cmd: "sudo -E rm -rf ~/important", want: deny},
		{cmd: "env rm -rf ~/important", want: deny},
		{cmd: "env FOO=1 rm -rf ~/important", want: deny},
		{cmd: "nice -n 5 rm -rf ~/important", want: deny},
		{cmd: "nohup rm -rf ~/important", want: deny},
		{cmd: "time rm -rf ~/important", want: deny},
		{cmd: "command rm -rf ~/important", want: deny},
		{cmd: "builtin rm -rf ~/important", atLeast: true, want: ask},
		{cmd: "exec rm -rf ~/important", want: deny},
		{cmd: "timeout 5 rm -rf ~/important", want: deny},
		{cmd: "/bin/rm -rf ~/important", want: deny},
		{cmd: "/usr/bin/env rm -rf ~/important", want: deny},
		{cmd: `\rm -rf ~/important`, want: deny},
		{cmd: `"rm" -rf ~/important`, want: deny},
		{cmd: `r''m -rf ~/important`, want: deny},
		{cmd: "FOO=bar rm -rf ~/important", want: deny},

		// Command lists, pipelines and grouping.
		{cmd: "true && rm -rf ~/important", want: deny},
		{cmd: "false || rm -rf ~/important", want: deny},
		{cmd: "ls; rm -rf ~/important", want: deny},
		{cmd: "echo x | rm -rf ~/important", want: deny},
		{cmd: "(rm -rf ~/important)", want: deny},
		{cmd: "{ rm -rf ~/important; }", want: deny},
		{cmd: "if true; then rm -rf ~/important; fi", want: deny},
		{cmd: "for d in ~/important; do rm -rf \"$d\"; done", want: deny},
		{cmd: "rm -rf ~/important &", want: deny},
		{cmd: "ls\nrm -rf ~/important", want: deny},

		// Nested shells and evaluation.
		{cmd: "bash -c 'rm -rf ~/important'", want: deny},
		{cmd: `sh -c "rm -rf $HOME/important"`, want: deny},
		{cmd: "bash -lc 'cd ~ && rm -rf important'", want: deny},
		{cmd: "zsh -c 'rm -rf ~/important'", want: deny},
		{cmd: "eval 'rm -rf ~/important'", want: deny},
		{cmd: `eval "rm -rf $HOME/important"`, want: deny},
		{cmd: "sudo bash -c 'rm -rf ~/important'", want: deny},
		{cmd: "echo 'rm -rf ~/important' | sh", atLeast: true, want: ask},
		{cmd: "bash <<'EOF'\nrm -rf ~/important\nEOF", atLeast: true, want: ask},
		{cmd: "echo $(rm -rf ~/important)", want: deny},
		{cmd: "x=`rm -rf ~/important`", want: deny},

		// Changing directory first.
		{cmd: "cd ~ && rm -rf important", want: deny},
		{cmd: "cd .. && rm -rf important", want: deny},
		{cmd: "cd ~; rm -rf *", want: deny},
		{cmd: "pushd ~ && rm -rf important", atLeast: true, want: ask},

		// Variables.
		{cmd: "d=~/important; rm -rf $d", want: deny},
		{cmd: "d=$HOME; rm -rf \"$d/important\"", want: deny},
		{cmd: "x=rm; $x -rf ~/important", want: deny},
		{cmd: "export T=~/important && rm -rf $T", want: deny},

		// Things that cannot be resolved before running.
		{cmd: "rm -rf $(cat list.txt)", atLeast: true, want: ask},
		{cmd: "rm -rf \"$UNDOGUARD_TEST_UNSET_VAR\"/", atLeast: true, want: ask},
		{cmd: "$(echo rm) -rf ~/important", atLeast: true, want: ask},

		// find and xargs.
		{cmd: "find ~ -delete", want: deny},
		{cmd: "find ~/important -name '*.txt' -delete", want: deny},
		{cmd: "find ~/important -exec rm {} \\;", atLeast: true, want: ask},
		{cmd: "find ~/important -type f -exec rm -f {} +", atLeast: true, want: ask},
		{cmd: "find ~/important | xargs rm -rf", atLeast: true, want: ask},
		{cmd: "echo ~/important | xargs rm -rf", atLeast: true, want: ask},

		// Copying and moving over things.
		{cmd: "mv README.md ~/notes.txt", want: deny},
		{cmd: "cp README.md ~/notes.txt", want: deny},
		{cmd: "mv ~/important /dev/null", atLeast: true, want: ask},
		{cmd: "echo > ~/notes.txt", want: deny},
		{cmd: "cat /dev/null > ~/notes.txt", want: deny},
		{cmd: "truncate -s 0 ~/notes.txt", want: deny},
		{cmd: "shred -u ~/notes.txt", want: deny},
		{cmd: "tee ~/notes.txt < README.md", want: deny},
		{cmd: "sed -i s/a/b/ ~/notes.txt", want: deny},
		{cmd: "sed -i '' s/a/b/ ~/notes.txt", want: deny},
		{cmd: "sed -i .bak -e s/a/b/ ~/notes.txt", want: deny},

		// Disks.
		{cmd: "dd if=/dev/zero of=/dev/sda", want: deny},
		{cmd: "mkfs.ext4 /dev/sdb1", want: deny},
		{cmd: "echo x > /dev/sda", want: deny},
		{cmd: "wipefs -a /dev/sdb", want: deny},

		// Things outside the local files.
		{cmd: "git push --force origin main", want: ask},
		{cmd: "git push -f", want: ask},
		{cmd: "docker system prune -af", want: ask},
		{cmd: "docker rm -f web", want: ask},
		{cmd: "docker rmi app:latest", want: ask},
		{cmd: "docker volume rm data", want: ask},
		{cmd: "kubectl delete namespace prod", want: ask},
		{cmd: "kubectl delete pod web-1", want: ask},
		{cmd: "systemctl stop nginx", want: ask},
		{cmd: "systemctl disable nginx", want: ask},
		{cmd: "systemctl mask nginx", want: ask},
		{cmd: `psql -c "DROP TABLE users"`, want: ask},
		{cmd: `psql -c "TRUNCATE users"`, want: ask},
		{cmd: `psql -c "DELETE FROM users"`, want: ask},
		{cmd: `mysql -e "DROP DATABASE shop"`, want: ask},
		{cmd: "psql <<'EOF'\nDROP TABLE users;\nEOF", want: ask},

		// A local SQLite file is a file like any other.
		{cmd: `sqlite3 ~/notes.txt "DELETE FROM users"`, want: deny},
	})
}

func TestLocalSQLiteIsSnapshotted(t *testing.T) {
	w := newWorld(t)
	db := filepath.Join(w.proj, "app.db")
	if err := os.WriteFile(db, []byte("not really a database"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := w.check(t, `sqlite3 app.db "DELETE FROM users"`)
	if d.Action != policy.Allow || !d.Destructive() {
		t.Fatalf("got %s", describe(d))
	}
	found := false
	for _, s := range d.Snapshot {
		found = found || s.Path == db
	}
	if !found {
		t.Errorf("snapshot %+v does not hold %s", d.Snapshot, db)
	}
}

// TestSafeLookalikes are commands that look dangerous but are not.
func TestSafeLookalikes(t *testing.T) {
	w := newWorld(t)
	runTable(t, w, []bypass{
		{cmd: "echo rm -rf ~", want: policy.Allow},
		{cmd: "grep -r 'rm -rf' src", want: policy.Allow},
		{cmd: "git log --grep 'reset --hard'", want: policy.Allow},
		{cmd: `psql -c "SELECT 1"`, want: policy.Allow},
		{cmd: `psql -c "DELETE FROM users WHERE id = 1"`, want: policy.Allow},
		{cmd: "docker ps -a", want: policy.Allow},
		{cmd: "kubectl get pods", want: policy.Allow},
		{cmd: "systemctl status nginx", want: policy.Allow},
		{cmd: "rm -rf build/out", want: policy.Allow},
		{cmd: "rm src/main.go", want: policy.Allow},
		{cmd: "git reset --hard", want: policy.Allow},
		{cmd: "git clean -fdx", want: policy.Allow},
		{cmd: "git checkout -- .", want: policy.Allow},
	})
}

func TestProjectRootAsks(t *testing.T) {
	w := newWorld(t)
	runTable(t, w, []bypass{
		{cmd: "rm -rf .", want: policy.Ask},
		{cmd: "rm -rf ../project", want: policy.Ask},
		{cmd: "rm -rf *", want: policy.Allow},
	})
}

func TestElsewhereAndScratch(t *testing.T) {
	w := newWorld(t)
	w.writePolicy(t, "scratch = [\""+filepath.Join(w.base, "scratch")+"\"]\n", "")
	runTable(t, w, []bypass{
		{cmd: "rm " + filepath.Join(w.base, "elsewhere.txt"), want: policy.Ask},
		{cmd: "rm " + filepath.Join(w.base, "scratch", "junk.log"), want: policy.Allow},
	})
	d := w.check(t, "rm "+filepath.Join(w.base, "scratch", "junk.log"))
	if len(d.Snapshot) != 0 {
		t.Errorf("scratch files are not snapshotted: %+v", d.Snapshot)
	}
}

func TestUserRules(t *testing.T) {
	w := newWorld(t)
	w.writePolicy(t, `
[[rule]]
name = "no force push"
command = "git push *"
action = "deny"
reason = "ask a human"

[[rule]]
name = "docker is fine"
finding = "docker-*"
action = "allow"

[[rule]]
path = "~/important/**"
action = "allow"
`, "")
	d := w.check(t, "git push --force origin main")
	if d.Action != policy.Deny || d.Deciding == nil || d.Deciding.By == nil || d.Deciding.By.Name != "no force push" {
		t.Fatalf("got %s", describe(d))
	}
	if !strings.Contains(d.Reason(), "ask a human") {
		t.Errorf("reason %q should carry the rule's reason", d.Reason())
	}
	if d := w.check(t, "docker volume rm data"); d.Action != policy.Allow {
		t.Errorf("docker rule: got %s", describe(d))
	}
	if d := w.check(t, "rm -rf ~/important"); d.Action != policy.Allow {
		t.Errorf("path rule: got %s", describe(d))
	}
	// A rule cannot loosen a protected path.
	if d := w.check(t, "rm -rf ~/.ssh"); d.Action != policy.Deny {
		t.Errorf("protected path: got %s", describe(d))
	}
}

func TestProjectRulesOnlyTighten(t *testing.T) {
	w := newWorld(t)
	w.writePolicy(t, "", `
[defaults]
home = "allow"

[[rule]]
command = "rm *"
path = "`+w.proj+`/src/**"
action = "deny"

[[rule]]
command = "kubectl *"
action = "allow"
`)
	p, err := policy.Load(w.proj)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Warnings) < 2 {
		t.Errorf("expected warnings for the loosening entries, got %q", p.Warnings)
	}
	if d := w.check(t, "rm src/main.go"); d.Action != policy.Deny {
		t.Errorf("project rule: got %s", describe(d))
	}
	if d := w.check(t, "rm -rf ~/important"); d.Action != policy.Deny {
		t.Errorf("project file loosened home: got %s", describe(d))
	}
	if d := w.check(t, "kubectl delete pod web-1"); d.Action != policy.Ask {
		t.Errorf("project file allowed kubectl: got %s", describe(d))
	}
}

func TestProjectPolicyFileIsGuarded(t *testing.T) {
	w := newWorld(t)
	w.writePolicy(t, "", "[[rule]]\ncommand = \"git push *\"\naction = \"ask\"\n")
	d := w.check(t, "rm "+policy.ProjectFileName)
	if d.Action != policy.Ask {
		t.Errorf("got %s", describe(d))
	}
}

func TestStoreAndConfigAreProtected(t *testing.T) {
	w := newWorld(t)
	for _, dir := range []string{w.data, w.conf} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if d := w.check(t, "rm -rf "+dir); d.Action != policy.Deny {
			t.Errorf("%s: got %s", dir, describe(d))
		}
	}
}

func TestSymlinkIntoHome(t *testing.T) {
	w := newWorld(t)
	link := filepath.Join(w.proj, "link")
	if err := os.Symlink(filepath.Join(w.home, "important"), link); err != nil {
		t.Skip("symlinks not available:", err)
	}
	// Removing the link itself is fine; going through it is not.
	if d := w.check(t, "rm link"); d.Action != policy.Allow {
		t.Errorf("rm link: got %s", describe(d))
	}
	if d := w.check(t, "rm -rf link/"); d.Action != policy.Deny {
		t.Errorf("rm -rf link/: got %s", describe(d))
	}
	if d := w.check(t, "echo x > link/thesis.txt"); d.Action != policy.Deny {
		t.Errorf("write through link: got %s", describe(d))
	}
}

func TestCheckArgv(t *testing.T) {
	w := newWorld(t)
	p, err := policy.Load(w.proj)
	if err != nil {
		t.Fatal(err)
	}
	d := CheckArgv([]string{"rm", "-rf", filepath.Join(w.home, "important")}, Options{Dir: w.proj, Policy: p})
	if d.Action != policy.Deny {
		t.Errorf("got %s", describe(d))
	}
	// An argument with spaces stays one word.
	d = CheckArgv([]string{"bash", "-c", "rm -rf ~/important"}, Options{Dir: w.proj, Policy: p})
	if d.Action != policy.Deny {
		t.Errorf("got %s", describe(d))
	}
}

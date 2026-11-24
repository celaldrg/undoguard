// Package paths holds the path helpers shared by the guard, the snapshot
// store and the CLI: well-known directories, lexical containment, symlink
// aware resolution and project root discovery.
package paths

import (
	"os"
	"path/filepath"
	"strings"
)

// Home returns the user's home directory. $HOME wins over the password
// database so that tests and wrappers can redirect it.
func Home() string {
	if h := os.Getenv("HOME"); h != "" && filepath.IsAbs(h) {
		return filepath.Clean(h)
	}
	h, err := os.UserHomeDir()
	if err != nil || h == "" {
		return ""
	}
	return filepath.Clean(h)
}

// ExpandTilde replaces a leading "~" or "~/" with the home directory.
func ExpandTilde(p string) string {
	if p == "~" {
		return Home()
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(Home(), p[2:])
	}
	return p
}

// Abs makes p absolute relative to dir and cleans it lexically. A leading
// tilde is expanded first.
func Abs(dir, p string) string {
	p = ExpandTilde(p)
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	return filepath.Clean(p)
}

// Within reports whether p equals root or lies below it. Both arguments
// must be clean absolute paths.
func Within(p, root string) bool {
	if root == "" || p == "" {
		return false
	}
	if p == root {
		return true
	}
	if root == string(filepath.Separator) {
		return strings.HasPrefix(p, root)
	}
	return strings.HasPrefix(p, root+string(filepath.Separator))
}

// Below reports whether p lies strictly below root.
func Below(p, root string) bool {
	return p != root && Within(p, root)
}

// Resolve returns the physical location of p. The longest existing prefix
// goes through filepath.EvalSymlinks and the missing remainder is appended
// unchanged. When follow is false the last element is kept as is, which is
// how rm, unlink and mv treat a symlink operand.
func Resolve(p string, follow bool) string {
	p = filepath.Clean(p)
	if !follow {
		dir, base := filepath.Split(p)
		if base == "" || dir == "" {
			return p
		}
		return filepath.Join(Resolve(filepath.Clean(dir), true), base)
	}
	var missing []string
	cur := p
	for {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			parts := make([]string, 0, len(missing)+1)
			parts = append(parts, r)
			for i := len(missing) - 1; i >= 0; i-- {
				parts = append(parts, missing[i])
			}
			return filepath.Clean(filepath.Join(parts...))
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		missing = append(missing, filepath.Base(cur))
		cur = parent
	}
}

// DataDir is where snapshots and the journal live: $UNDOGUARD_HOME, else
// $XDG_DATA_HOME/undoguard, else ~/.local/share/undoguard.
func DataDir() string {
	if d := os.Getenv("UNDOGUARD_HOME"); d != "" && filepath.IsAbs(d) {
		return filepath.Clean(d)
	}
	if d := os.Getenv("XDG_DATA_HOME"); d != "" && filepath.IsAbs(d) {
		return filepath.Join(d, "undoguard")
	}
	return filepath.Join(Home(), ".local", "share", "undoguard")
}

// ConfigDir holds the global policy file: $UNDOGUARD_CONFIG_DIR, else
// $XDG_CONFIG_HOME/undoguard, else ~/.config/undoguard.
func ConfigDir() string {
	if d := os.Getenv("UNDOGUARD_CONFIG_DIR"); d != "" && filepath.IsAbs(d) {
		return filepath.Clean(d)
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" && filepath.IsAbs(d) {
		return filepath.Join(d, "undoguard")
	}
	return filepath.Join(Home(), ".config", "undoguard")
}

// ProjectRoot picks the directory treated as the project. An explicit
// UNDOGUARD_PROJECT or the project directory an agent exports wins, then
// the enclosing git work tree, then cwd itself.
func ProjectRoot(cwd string) string {
	for _, k := range []string{"UNDOGUARD_PROJECT", "CLAUDE_PROJECT_DIR", "GEMINI_PROJECT_DIR"} {
		if v := os.Getenv(k); v != "" && filepath.IsAbs(v) {
			return filepath.Clean(v)
		}
	}
	if r := GitRoot(cwd); r != "" {
		return r
	}
	return filepath.Clean(cwd)
}

// GitRoot walks up from dir looking for a .git directory or file and
// returns the directory that holds it, or "" when there is none.
func GitRoot(dir string) string {
	if dir == "" {
		return ""
	}
	dir = filepath.Clean(dir)
	for {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// ScratchDirs lists directories whose contents are disposable: the system
// temporary directories in both their written and their physical form.
func ScratchDirs() []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if p == "" || !filepath.IsAbs(p) {
			return
		}
		for _, q := range []string{filepath.Clean(p), Resolve(p, true)} {
			if q == string(filepath.Separator) || seen[q] {
				continue
			}
			seen[q] = true
			out = append(out, q)
		}
	}
	add(os.Getenv("TMPDIR"))
	add(os.TempDir())
	add("/tmp")
	add("/var/tmp")
	return out
}

// Exists reports whether p exists without following a final symlink.
func Exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// IsDir reports whether p is a directory, following symlinks.
func IsDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// Display shortens p for humans by replacing the home directory with "~".
func Display(p string) string {
	h := Home()
	if h == "" || h == "/" {
		return p
	}
	if p == h {
		return "~"
	}
	if Below(p, h) {
		return "~" + p[len(h):]
	}
	return p
}

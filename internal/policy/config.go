// Package policy loads the user's policy file and the optional project
// file, and decides what happens to each path or resource a command
// touches.
package policy

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/celaldrg/undoguard/internal/paths"
)

// Action is what the guard does with a command.
type Action string

// The actions, from loosest to strictest.
const (
	Allow Action = "allow"
	Ask   Action = "ask"
	Deny  Action = "deny"
)

// Rank orders actions by strictness.
func (a Action) Rank() int {
	switch a {
	case Allow:
		return 0
	case Ask:
		return 1
	default:
		return 2
	}
}

// Stricter returns the stricter of a and b.
func Stricter(a, b Action) Action {
	if b.Rank() > a.Rank() {
		return b
	}
	return a
}

func parseAction(s string) (Action, error) {
	switch a := Action(strings.ToLower(strings.TrimSpace(s))); a {
	case Allow, Ask, Deny:
		return a, nil
	case "":
		return "", nil
	}
	return "", fmt.Errorf("unknown action %q: use allow, ask or deny", s)
}

// Defaults holds the action for each class of target.
type Defaults struct {
	// Project is for paths inside the project. They are snapshotted.
	Project string `toml:"project"`
	// ProjectRoot is for the project directory itself.
	ProjectRoot string `toml:"project_root"`
	// Scratch is for the system temporary directories.
	Scratch string `toml:"scratch"`
	// Home is for the home directory outside the project.
	Home string `toml:"home"`
	// Elsewhere is for any other path.
	Elsewhere string `toml:"elsewhere"`
	// Unresolved is for paths and code that cannot be worked out before
	// the command runs.
	Unresolved string `toml:"unresolved"`
	// External is for databases, containers, clusters, services and
	// remote repositories, which cannot be snapshotted.
	External string `toml:"external"`
	// Critical is for disks, partitions and file systems.
	Critical string `toml:"critical"`
}

// SnapshotConfig limits the snapshot store.
type SnapshotConfig struct {
	// MaxFileSize is the largest single file that is copied, such as
	// "512MiB".
	MaxFileSize string `toml:"max_file_size"`
	// MaxTotalSize caps the bytes one command may need snapshotted.
	MaxTotalSize string `toml:"max_total_size"`
	// MaxFiles caps the paths (files, directories and links) one command
	// may need snapshotted. Zero means no cap, as for the sizes.
	MaxFiles int `toml:"max_files"`
	// StoreLimit is the size gc trims the store down to.
	StoreLimit string `toml:"store_limit"`
	// KeepDays is how long snapshots are kept.
	KeepDays int `toml:"keep_days"`
	// KeepMin is how many of the newest snapshots are always kept.
	KeepMin int `toml:"keep_min"`
}

// Rule is one user rule. Every field that is set must match.
type Rule struct {
	// Name labels the rule in output.
	Name string `toml:"name"`
	// Command is a pattern for a simple command, written as the program
	// name followed by its arguments, such as "git push *".
	Command string `toml:"command"`
	// Finding is a pattern for the detector rule, such as "docker-*".
	Finding string `toml:"finding"`
	// Path is a pattern for a target path, such as "~/work/**".
	Path string `toml:"path"`
	// Action is allow, ask or deny.
	Action string `toml:"action"`
	// Reason is shown when the rule decides.
	Reason string `toml:"reason"`
}

// File is the policy file as written.
type File struct {
	// Protected lists paths that are always denied, on top of the
	// built-in list.
	Protected []string `toml:"protected"`
	// Unprotected removes entries from the built-in list. Only the user
	// policy may do this.
	Unprotected []string `toml:"unprotected"`
	// Scratch lists extra disposable directories.
	Scratch []string `toml:"scratch"`
	// Defaults sets the action for each class.
	Defaults Defaults `toml:"defaults"`
	// Snapshot sets the store limits.
	Snapshot SnapshotConfig `toml:"snapshot"`
	// Rules are matched against every command and finding.
	Rules []Rule `toml:"rule"`
}

// DefaultProtected are the paths protected out of the box, beyond the
// home directory as a whole.
var DefaultProtected = []string{
	"~/.ssh", "~/.aws", "~/.gnupg", "~/.config", "~/.kube", "~/.docker", "~/.azure",
	"~/.netrc", "~/.git-credentials", "~/.npmrc", "~/.pypirc", "~/.local/share/keyrings",
	"/etc", "/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/boot", "/proc", "/sys",
	"/System", "/Library", "/Applications", "/private/etc",
}

// Defaulted returns the built-in policy.
func Defaulted() File {
	return File{
		Protected: append([]string(nil), DefaultProtected...),
		Defaults: Defaults{
			Project:     string(Allow),
			ProjectRoot: string(Ask),
			Scratch:     string(Allow),
			Home:        string(Deny),
			Elsewhere:   string(Ask),
			Unresolved:  string(Ask),
			External:    string(Ask),
			Critical:    string(Deny),
		},
		Snapshot: SnapshotConfig{
			MaxFileSize:  "512MiB",
			MaxTotalSize: "2GiB",
			MaxFiles:     50000,
			StoreLimit:   "10GiB",
			KeepDays:     14,
			KeepMin:      20,
		},
	}
}

// Limits are the parsed snapshot limits.
type Limits struct {
	MaxFileSize  int64
	MaxTotalSize int64
	MaxFiles     int
	StoreLimit   int64
	KeepDays     int
	KeepMin      int
}

// Classes is the parsed action per class.
type Classes struct {
	Project     Action
	ProjectRoot Action
	Scratch     Action
	Home        Action
	Elsewhere   Action
	Unresolved  Action
	External    Action
	Critical    Action
}

// Policy is the loaded, merged and checked policy.
type Policy struct {
	// Project is the project root, or "" when there is none.
	Project string
	// Home is the home directory.
	Home string
	// Classes holds the action per class.
	Classes Classes
	// Protected holds absolute protected paths, in written and physical
	// form.
	Protected []string
	// Scratch holds the disposable directories.
	Scratch []string
	// Rules from the user file come first, then those of the project.
	Rules []LoadedRule
	// Limits are the snapshot limits.
	Limits Limits
	// Sources lists the files that were read.
	Sources []string
	// Warnings lists what was ignored while loading.
	Warnings []string
}

// LoadedRule is a checked rule and where it came from.
type LoadedRule struct {
	Rule
	Act    Action
	Source string
	Index  int
	// Project is set for rules from the project file.
	Project bool

	command, finding, path *regexp.Regexp
}

// Label names the rule for output.
func (r *LoadedRule) Label() string {
	if r.Name != "" {
		return r.Name
	}
	return fmt.Sprintf("%s rule %d", filepath.Base(r.Source), r.Index+1)
}

// ProjectFileName is the name of the optional per-project policy file.
const ProjectFileName = ".undoguard.toml"

// UserFile returns the path of the user's policy file.
func UserFile() string {
	return filepath.Join(paths.ConfigDir(), "policy.toml")
}

// Load reads the user policy and the project policy for a command run in
// cwd. A missing file is not an error.
func Load(cwd string) (*Policy, error) {
	return LoadFiles(UserFile(), cwd)
}

// LoadFiles is Load with an explicit user policy file.
func LoadFiles(userFile, cwd string) (*Policy, error) {
	user := Defaulted()
	// Listed paths add to the built-in ones instead of replacing them.
	user.Protected = nil
	p := &Policy{Home: paths.Home()}
	if err := decodeInto(userFile, &user, p); err != nil {
		return nil, err
	}
	if root := paths.ProjectRoot(cwd); root != "" {
		p.Project = root
	}
	h := p.Home
	if p.Project != "" && (h == "" || paths.Within(h, p.Project)) {
		// A project at or above home would make all of home fair game.
		p.Warnings = append(p.Warnings, fmt.Sprintf("%s is the home directory or above it, so it is not treated as a project", paths.Display(p.Project)))
		p.Project = ""
	}
	var proj File
	projFile := ""
	if p.Project != "" {
		projFile = filepath.Join(p.Project, ProjectFileName)
		if err := decodeInto(projFile, &proj, p); err != nil {
			return nil, err
		}
	}
	if err := p.build(user, userFile, proj, projFile); err != nil {
		return nil, err
	}
	return p, nil
}

func decodeInto(file string, f *File, p *Policy) error {
	data, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read policy: %w", err)
	}
	md, err := toml.Decode(string(data), f)
	if err != nil {
		return fmt.Errorf("%s: %w", paths.Display(file), err)
	}
	for _, k := range md.Undecoded() {
		p.Warnings = append(p.Warnings, fmt.Sprintf("%s: unknown key %q ignored", paths.Display(file), k.String()))
	}
	p.Sources = append(p.Sources, file)
	return nil
}

// build checks the user file, then lets the project file tighten it.
func (p *Policy) build(user File, userFile string, proj File, projFile string) error {
	var err error
	if p.Classes, err = classes(user.Defaults, userFile, builtinClasses()); err != nil {
		return err
	}
	projClasses, err := classes(proj.Defaults, projFile, Classes{})
	if err != nil {
		return err
	}
	// The project file comes from the repository the agent works in, so
	// it can only make things stricter.
	tighten := func(dst *Action, src Action, key string) {
		if src == "" {
			return
		}
		if src.Rank() < dst.Rank() {
			p.Warnings = append(p.Warnings, fmt.Sprintf("%s: defaults.%s = %q is looser than the user policy and is ignored", ProjectFileName, key, src))
			return
		}
		*dst = src
	}
	tighten(&p.Classes.Project, projClasses.Project, "project")
	tighten(&p.Classes.ProjectRoot, projClasses.ProjectRoot, "project_root")
	tighten(&p.Classes.Scratch, projClasses.Scratch, "scratch")
	tighten(&p.Classes.Home, projClasses.Home, "home")
	tighten(&p.Classes.Elsewhere, projClasses.Elsewhere, "elsewhere")
	tighten(&p.Classes.Unresolved, projClasses.Unresolved, "unresolved")
	tighten(&p.Classes.External, projClasses.External, "external")
	tighten(&p.Classes.Critical, projClasses.Critical, "critical")

	removed := map[string]bool{}
	for _, e := range user.Unprotected {
		removed[expand(e)] = true
	}
	if len(proj.Unprotected) > 0 {
		p.Warnings = append(p.Warnings, ProjectFileName+": unprotected can only be set in the user policy")
	}
	for _, e := range DefaultProtected {
		if !removed[expand(e)] {
			p.addProtected(e)
		}
	}
	for _, e := range append(append([]string(nil), user.Protected...), proj.Protected...) {
		p.addProtected(e)
	}
	// The snapshot store and the policy itself must survive the agent.
	p.addProtected(paths.DataDir())
	p.addProtected(paths.ConfigDir())
	if len(proj.Scratch) > 0 {
		p.Warnings = append(p.Warnings, ProjectFileName+": scratch directories can only be set in the user policy")
	}
	p.Scratch = p.scratchDirs(user.Scratch)

	if err := p.addRules(user.Rules, userFile, true); err != nil {
		return err
	}
	if err := p.addRules(proj.Rules, projFile, false); err != nil {
		return err
	}
	if len(proj.Snapshot.MaxFileSize+proj.Snapshot.MaxTotalSize+proj.Snapshot.StoreLimit) > 0 ||
		proj.Snapshot.MaxFiles != 0 || proj.Snapshot.KeepDays != 0 || proj.Snapshot.KeepMin != 0 {
		p.Warnings = append(p.Warnings, ProjectFileName+": snapshot limits can only be set in the user policy")
	}
	p.Limits, err = limits(user.Snapshot)
	return err
}

func builtinClasses() Classes {
	return Classes{Project: Allow, ProjectRoot: Ask, Scratch: Allow, Home: Deny, Elsewhere: Ask, Unresolved: Ask, External: Ask, Critical: Deny}
}

// classes parses the defaults table. Keys left empty keep base.
func classes(d Defaults, file string, base Classes) (Classes, error) {
	c := base
	var err error
	set := func(dst *Action, v, key string) {
		if err != nil || strings.TrimSpace(v) == "" {
			return
		}
		a, e := parseAction(v)
		if e != nil {
			err = fmt.Errorf("%s: defaults.%s: %w", paths.Display(file), key, e)
			return
		}
		*dst = a
	}
	set(&c.Project, d.Project, "project")
	set(&c.ProjectRoot, d.ProjectRoot, "project_root")
	set(&c.Scratch, d.Scratch, "scratch")
	set(&c.Home, d.Home, "home")
	set(&c.Elsewhere, d.Elsewhere, "elsewhere")
	set(&c.Unresolved, d.Unresolved, "unresolved")
	set(&c.External, d.External, "external")
	set(&c.Critical, d.Critical, "critical")
	return c, err
}

// expand turns a configured path into an absolute one. It understands a
// leading "~" and $HOME.
func expand(p string) string {
	p = strings.TrimSpace(p)
	if strings.HasPrefix(p, "$HOME") {
		p = paths.Home() + p[len("$HOME"):]
	} else if strings.HasPrefix(p, "${HOME}") {
		p = paths.Home() + p[len("${HOME}"):]
	}
	p = paths.ExpandTilde(p)
	if !filepath.IsAbs(p) {
		return ""
	}
	return filepath.Clean(p)
}

func (p *Policy) addProtected(e string) {
	abs := expand(e)
	if abs == "" {
		if strings.TrimSpace(e) != "" {
			p.Warnings = append(p.Warnings, fmt.Sprintf("protected path %q is not absolute and is ignored", e))
		}
		return
	}
	for _, q := range []string{abs, paths.Resolve(abs, true)} {
		if !contains(p.Protected, q) {
			p.Protected = append(p.Protected, q)
		}
	}
}

// scratchDirs returns the disposable directories. One that holds the home
// directory or the project would make them disposable too, so it is left
// out.
func (p *Policy) scratchDirs(extra []string) []string {
	var out []string
	cands := paths.ScratchDirs()
	for _, e := range extra {
		if abs := expand(e); abs != "" {
			cands = append(cands, abs, paths.Resolve(abs, true))
		}
	}
	for _, d := range cands {
		if d == "/" || contains(out, d) {
			continue
		}
		if p.Home != "" && paths.Within(p.Home, d) {
			continue
		}
		if p.Project != "" && paths.Within(p.Project, d) {
			continue
		}
		out = append(out, d)
	}
	return out
}

func (p *Policy) addRules(rules []Rule, file string, user bool) error {
	for i, r := range rules {
		a, err := parseAction(r.Action)
		if err != nil {
			return fmt.Errorf("%s: rule %d: %w", paths.Display(file), i+1, err)
		}
		if a == "" {
			return fmt.Errorf("%s: rule %d: action is required", paths.Display(file), i+1)
		}
		if r.Command == "" && r.Finding == "" && r.Path == "" {
			return fmt.Errorf("%s: rule %d: set at least one of command, finding or path", paths.Display(file), i+1)
		}
		if r.Path != "" && expand(r.Path) == "" {
			return fmt.Errorf("%s: rule %d: path pattern must be absolute or start with ~", paths.Display(file), i+1)
		}
		if !user && a == Allow {
			p.Warnings = append(p.Warnings, fmt.Sprintf("%s: rule %d allows and is ignored, project rules can only ask or deny", ProjectFileName, i+1))
			continue
		}
		lr := LoadedRule{Rule: r, Act: a, Source: file, Index: i, Project: !user}
		if err := lr.compile(); err != nil {
			return fmt.Errorf("%s: rule %d: %w", paths.Display(file), i+1, err)
		}
		p.Rules = append(p.Rules, lr)
	}
	return nil
}

func limits(s SnapshotConfig) (Limits, error) {
	l := Limits{MaxFiles: s.MaxFiles, KeepDays: s.KeepDays, KeepMin: s.KeepMin}
	var err error
	if l.MaxFileSize, err = ParseSize(s.MaxFileSize); err != nil {
		return l, fmt.Errorf("snapshot.max_file_size: %w", err)
	}
	if l.MaxTotalSize, err = ParseSize(s.MaxTotalSize); err != nil {
		return l, fmt.Errorf("snapshot.max_total_size: %w", err)
	}
	if l.StoreLimit, err = ParseSize(s.StoreLimit); err != nil {
		return l, fmt.Errorf("snapshot.store_limit: %w", err)
	}
	if l.MaxFiles < 0 || l.KeepDays < 0 || l.KeepMin < 0 {
		return l, errors.New("snapshot limits must not be negative")
	}
	return l, nil
}

// DefaultLimits returns the snapshot limits of the built-in policy, for
// commands that must work while a policy file is broken.
func DefaultLimits() Limits {
	l, _ := limits(Defaulted().Snapshot)
	return l
}

// ParseSize reads sizes such as "512MiB", "2GB", "100k" or "4096".
// Units are powers of 1024 whether or not they carry an "i".
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	num := strings.TrimRightFunc(s, func(r rune) bool { return r < '0' || r > '9' })
	unit := strings.ToLower(strings.TrimSpace(s[len(num):]))
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("bad size %q", s)
	}
	var mult int64
	switch strings.TrimSuffix(strings.TrimSuffix(unit, "b"), "i") {
	case "":
		mult = 1
	case "k":
		mult = 1 << 10
	case "m":
		mult = 1 << 20
	case "g":
		mult = 1 << 30
	case "t":
		mult = 1 << 40
	default:
		return 0, fmt.Errorf("bad size unit in %q", s)
	}
	if n > (1<<62)/mult {
		return 0, fmt.Errorf("size %q is too large", s)
	}
	return n * mult, nil
}

// FormatSize renders a byte count for humans.
func FormatSize(n int64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	f := float64(n)
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

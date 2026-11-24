package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/celaldrg/undoguard/internal/guard"
	"github.com/celaldrg/undoguard/internal/paths"
	"github.com/celaldrg/undoguard/internal/policy"
)

func cmdCheck(a *app, args []string) int {
	fs := a.flags("check", "[--json] [--dir DIR] '<command line>'  |  undoguard check [--json] -- <program> <args>...")
	asJSON := fs.Bool("json", false, "print the decision as JSON")
	dirFlag := fs.String("dir", "", "directory the command runs in (default: the current directory)")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return exitError
	}
	dir, err := workDir(*dirFlag)
	if err != nil {
		return a.errorf("%v", err)
	}
	p, err := a.loadPolicy(dir, *asJSON)
	if err != nil {
		return a.errorf("%v", err)
	}
	o := guard.Options{Dir: dir, Policy: p}
	var d *guard.Decision
	if len(rest) == 1 {
		d = guard.Check(rest[0], o)
	} else {
		d = guard.CheckArgv(rest, o)
	}
	if *asJSON {
		enc := json.NewEncoder(a.stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(decisionJSON(d)); err != nil {
			return a.errorf("%v", err)
		}
	} else {
		printDecision(a.stdout, d)
	}
	return exitCode(d.Action)
}

func exitCode(act policy.Action) int {
	switch act {
	case policy.Allow:
		return exitOK
	case policy.Ask:
		return exitAsk
	default:
		return exitDeny
	}
}

// targetCell renders an assessment's target for the table.
func targetCell(as *guard.Assessment) string {
	s := as.Subject()
	if s == "" {
		s = "-"
	}
	var notes []string
	t := as.Target
	if t.Unresolved && t.Raw != "" {
		notes = append(notes, "unresolved")
	}
	if t.Recursive && t.Path != "" {
		notes = append(notes, "recursive")
	}
	if t.Git != "" {
		notes = append(notes, "git "+string(t.Git)+" files")
	}
	if t.Follow && t.Path != "" {
		notes = append(notes, "follows symlinks")
	}
	if len(notes) > 0 {
		s += " (" + strings.Join(notes, ", ") + ")"
	}
	return s
}

func printDecision(w io.Writer, d *guard.Decision) {
	fmt.Fprintf(w, "Decision: %s\n", d.Action)
	fmt.Fprintf(w, "Reason:   %s\n", d.Reason())
	if len(d.Assessments) > 0 {
		fmt.Fprintln(w)
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  ACTION\tOP\tCLASS\tTARGET\tDECIDED BY")
		for i := range d.Assessments {
			as := &d.Assessments[i]
			op, class := string(as.Op), string(as.Class)
			if op == "" {
				op = "-"
			}
			if class == "" {
				class = "-"
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", as.Action, op, class, targetCell(as), as.Decider())
		}
		tw.Flush()
		fmt.Fprintln(w)
		for i := range d.Assessments {
			as := &d.Assessments[i]
			line := as.Rule
			if as.Command != "" {
				line = as.Command
			}
			fmt.Fprintf(w, "  %s: %s", line, as.What)
			if as.Why != "" {
				fmt.Fprintf(w, "; %s", as.Why)
			}
			fmt.Fprintln(w)
		}
	}
	switch {
	case d.Action == policy.Deny:
	case len(d.Snapshot) == 0:
		if d.Destructive() {
			fmt.Fprintln(w, "\nNothing to snapshot.")
		}
	default:
		fmt.Fprintln(w, "\nSnapshot before running:")
		for _, s := range d.Snapshot {
			note := ""
			if s.Git != "" {
				note = " (git " + string(s.Git) + " files)"
			} else if s.Recursive {
				note = " (recursive)"
			}
			if !paths.Exists(s.Path) {
				note += " (does not exist yet)"
			}
			fmt.Fprintf(w, "  %s%s\n", paths.Display(s.Path), note)
		}
	}
}

type jsonAssessment struct {
	Action    string `json:"action"`
	Op        string `json:"op,omitempty"`
	Rule      string `json:"rule"`
	Command   string `json:"command,omitempty"`
	What      string `json:"what,omitempty"`
	Path      string `json:"path,omitempty"`
	Raw       string `json:"raw,omitempty"`
	Recursive bool   `json:"recursive,omitempty"`
	Git       string `json:"git,omitempty"`
	Resource  string `json:"resource,omitempty"`
	Class     string `json:"class,omitempty"`
	Why       string `json:"why,omitempty"`
	DecidedBy string `json:"decided_by"`
}

type jsonDecision struct {
	Command  string           `json:"command"`
	Dir      string           `json:"dir"`
	Decision string           `json:"decision"`
	Reason   string           `json:"reason"`
	Findings []jsonAssessment `json:"findings"`
	Snapshot []string         `json:"snapshot"`
	Warnings []string         `json:"warnings,omitempty"`
}

func decisionJSON(d *guard.Decision) jsonDecision {
	out := jsonDecision{
		Command:  d.Command,
		Dir:      d.Dir,
		Decision: string(d.Action),
		Reason:   d.Reason(),
		Findings: []jsonAssessment{},
		Snapshot: []string{},
		Warnings: d.Warnings,
	}
	for i := range d.Assessments {
		as := &d.Assessments[i]
		out.Findings = append(out.Findings, jsonAssessment{
			Action:    string(as.Action),
			Op:        string(as.Op),
			Rule:      as.Rule,
			Command:   as.Command,
			What:      as.What,
			Path:      as.Target.Path,
			Raw:       as.Target.Raw,
			Recursive: as.Target.Recursive,
			Git:       string(as.Target.Git),
			Resource:  as.Resource,
			Class:     string(as.Class),
			Why:       as.Why,
			DecidedBy: as.Decider(),
		})
	}
	if d.Action != policy.Deny {
		for _, s := range d.Snapshot {
			out.Snapshot = append(out.Snapshot, s.Path)
		}
	}
	return out
}

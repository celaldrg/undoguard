package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/celaldrg/undoguard/internal/paths"
	"github.com/celaldrg/undoguard/internal/policy"
)

func cmdPolicy(a *app, args []string) int {
	fs := a.flags("policy", "[--dir DIR]")
	dirFlag := fs.String("dir", "", "directory whose project policy to include (default: the current directory)")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	dir, err := workDir(*dirFlag)
	if err != nil {
		return a.errorf("%v", err)
	}
	p, err := a.loadPolicy(dir, true)
	if err != nil {
		return a.errorf("%v", err)
	}
	w := a.stdout
	fmt.Fprintf(w, "User policy:    %s\n", paths.Display(policy.UserFile()))
	if len(p.Sources) == 0 {
		fmt.Fprintln(w, "Files read:     none, built-in defaults only")
	} else {
		for i, s := range p.Sources {
			label := "Files read:    "
			if i > 0 {
				label = "               "
			}
			fmt.Fprintf(w, "%s %s\n", label, paths.Display(s))
		}
	}
	project := "none"
	if p.Project != "" {
		project = paths.Display(p.Project)
	}
	fmt.Fprintf(w, "Project:        %s\n", project)
	fmt.Fprintf(w, "Snapshot store: %s\n\n", paths.Display(paths.DataDir()))

	fmt.Fprintln(w, "Defaults:")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, c := range []policy.Class{
		policy.ClassProject, policy.ClassProjectRoot, policy.ClassScratch, policy.ClassHome,
		policy.ClassElsewhere, policy.ClassUnresolved, policy.ClassExternal, policy.ClassCritical,
		policy.ClassProtected, policy.ClassPolicyFile,
	} {
		fmt.Fprintf(tw, "  %s\t%s\n", c, p.Action(c))
	}
	tw.Flush()

	fmt.Fprintln(w, "\nProtected paths:")
	for _, pr := range p.Protected {
		fmt.Fprintf(w, "  %s\n", paths.Display(pr))
	}
	fmt.Fprintln(w, "\nScratch directories:")
	for _, s := range p.Scratch {
		fmt.Fprintf(w, "  %s\n", s)
	}
	if len(p.Rules) > 0 {
		fmt.Fprintln(w, "\nRules:")
		tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  NAME\tACTION\tCOMMAND\tFINDING\tPATH\tFROM")
		for i := range p.Rules {
			r := &p.Rules[i]
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\n", r.Label(), r.Act, dash(r.Command), dash(r.Finding), dash(r.Path), paths.Display(r.Source))
		}
		tw.Flush()
	}
	l := p.Limits
	fmt.Fprintln(w, "\nSnapshot limits:")
	fmt.Fprintf(w, "  largest file      %s\n", policy.FormatSize(l.MaxFileSize))
	fmt.Fprintf(w, "  total per command %s\n", policy.FormatSize(l.MaxTotalSize))
	fmt.Fprintf(w, "  paths per command %d\n", l.MaxFiles)
	fmt.Fprintf(w, "  store size        %s\n", policy.FormatSize(l.StoreLimit))
	fmt.Fprintf(w, "  keep              %d days, and always the newest %d\n", l.KeepDays, l.KeepMin)
	if len(p.Warnings) > 0 {
		fmt.Fprintln(w, "\nWarnings:")
		for _, x := range p.Warnings {
			fmt.Fprintf(w, "  %s\n", x)
		}
	}
	return exitOK
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

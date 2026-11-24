package detect

import (
	"strings"

	"github.com/celaldrg/undoguard/internal/parser"
)

// extractInto is the finding of an archive extraction into dir: any file
// in it may be replaced by one from the archive.
func extractInto(c *parser.Command, rule string, dir Target) []Finding {
	dir.Recursive = true
	dir.Follow = true
	return []Finding{{
		Op:      OpOverwrite,
		Rule:    rule,
		What:    "extracts an archive over what is already in the directory",
		Command: c,
		Targets: []Target{dir},
	}}
}

// tar finds extractions, in the dashed (-xf), long (--extract) and old
// bundled (xzf) forms, into -C / --directory or the working directory.
func tar(c *parser.Command) []Finding {
	args := c.Args
	extract := false
	if len(args) > 0 && !args[0].Dynamic && args[0].Value != "" && !strings.HasPrefix(args[0].Value, "-") &&
		strings.Trim(args[0].Value, "AcdrtuxfjzJavkpPsSOmwWlhiBb0123456789") == "" {
		// The old form bundles the letters without a dash; f and the
		// other letters that take a value read the next arguments.
		extract = strings.Contains(args[0].Value, "x")
		if strings.Contains(args[0].Value, "k") {
			return nil
		}
		args = args[1:]
		if strings.Contains(c.Args[0].Value, "f") && len(args) > 0 {
			args = args[1:]
		}
	}
	o := parseOpts(args, "fCbTXIgHKLNV", "file", "directory", "blocking-factor", "files-from", "exclude-from",
		"use-compress-program", "listed-incremental", "format", "starting-file", "newer", "label", "transform", "xform",
		"exclude", "owner", "group", "mode", "mtime")
	if o.has("x", "extract", "get") {
		extract = true
	}
	if !extract || o.has("t", "list", "help", "version", "k", "keep-old-files", "skip-old-files", "to-stdout", "O") {
		return nil
	}
	dirs := o.all("C", "directory")
	if len(dirs) == 0 {
		return extractInto(c, "tar-extract", cwdTarget(c))
	}
	// -C changes directory for what follows, relative to the one before.
	last := dirs[len(dirs)-1]
	t := target(c, last)
	if !t.Unresolved && len(dirs) > 1 && !strings.HasPrefix(last.Value, "/") {
		for i := len(dirs) - 2; i >= 0; i-- {
			if strings.HasPrefix(dirs[i].Value, "/") || i == 0 {
				base := target(c, dirs[i])
				if base.Unresolved {
					t = base
					break
				}
				p := base.Path
				for _, d := range dirs[i+1:] {
					p = joinUnder(p, d.Value)
				}
				t = Target{Path: p, Raw: last.Value}
				break
			}
		}
	}
	return extractInto(c, "tar-extract", t)
}

func joinUnder(dir, p string) string {
	if strings.HasPrefix(p, "/") {
		return p
	}
	return strings.TrimRight(dir, "/") + "/" + p
}

// unzip finds extractions that replace existing files without asking:
// -o, and -f / -u, which freshen or update existing files.
func unzip(c *parser.Command) []Finding {
	o := parseOpts(c.Args, "dxPI", "")
	if o.has("l", "t", "v", "p", "c", "z", "Z", "n", "h", "hh") || !o.has("o", "f", "u") {
		return nil
	}
	if w, ok := o.word("d"); ok {
		return extractInto(c, "unzip", target(c, w))
	}
	return extractInto(c, "unzip", cwdTarget(c))
}

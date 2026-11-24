package detect

import (
	"os"
	"regexp"
	"strings"

	"github.com/celaldrg/undoguard/internal/parser"
)

// subcommand skips leading options and returns the first operand and what
// follows it.
func subcommand(args []parser.Word, shortVal string, longVal ...string) (parser.Word, []parser.Word, bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		v := a.Value
		if a.Dynamic || !strings.HasPrefix(v, "-") || v == "-" {
			return a, args[i+1:], true
		}
		if v == "--" {
			if i+1 < len(args) {
				return args[i+1], args[i+2:], true
			}
			return parser.Word{}, nil, false
		}
		if strings.HasPrefix(v, "--") {
			name, _, hasVal := strings.Cut(v[2:], "=")
			if !hasVal {
				for _, l := range longVal {
					if l == name {
						i++
						break
					}
				}
			}
			continue
		}
		// A short option that takes a value as the last letter of its
		// cluster consumes the next argument.
		for j := 1; j < len(v); j++ {
			if strings.IndexByte(shortVal, v[j]) >= 0 {
				if j+1 == len(v) {
					i++
				}
				break
			}
		}
	}
	return parser.Word{}, nil, false
}

func extFinding(c *parser.Command, rule, what, resource string) Finding {
	return Finding{Op: OpExternal, Rule: rule, What: what, Command: c, Resource: resource}
}

var containerGroups = setOf(
	"container", "image", "volume", "network", "system", "builder", "buildx", "pod", "machine",
	"secret", "config", "context", "plugin", "node", "service", "stack", "manifest", "artifact",
)

var containerGroupActions = setOf("rm", "remove", "prune", "delete", "reset", "rmi")

func container(prog string) detector {
	return func(c *parser.Command) []Finding {
		sub, rest, ok := subcommand(c.Args, "cHl", "config", "context", "host", "log-level", "tlscacert",
			"tlscert", "tlskey", "url", "connection", "identity", "root", "runroot", "storage-driver", "namespace")
		if !ok || sub.Dynamic {
			return nil
		}
		switch s := sub.Value; {
		case s == "rm":
			return []Finding{extFinding(c, prog+"-rm", "removes containers", prog+" container")}
		case s == "rmi":
			return []Finding{extFinding(c, prog+"-rmi", "removes images", prog+" image")}
		case s == "compose":
			return composeFindings(c, prog, rest)
		case containerGroups[s]:
			act, _, ok := subcommand(rest, "f", "filter", "format")
			if !ok {
				return nil
			}
			if act.Dynamic || containerGroupActions[act.Value] {
				return []Finding{extFinding(c, prog+"-"+s+"-"+act.Value, "removes "+s+" objects", prog+" "+s)}
			}
		}
		return nil
	}
}

// compose handles the stand-alone docker-compose and podman-compose
// programs.
func compose(c *parser.Command) []Finding {
	return composeFindings(c, strings.TrimSuffix(c.Name, "-compose"), c.Args)
}

func composeFindings(c *parser.Command, prog string, args []parser.Word) []Finding {
	act, rest, ok := subcommand(args, "fp", "file", "project-name", "project-directory", "profile", "env-file",
		"ansi", "progress", "parallel")
	if !ok {
		return nil
	}
	o := parseOpts(rest, "t", "timeout", "rmi")
	switch {
	case act.Dynamic:
		return []Finding{extFinding(c, prog+"-compose", "runs a compose command only known at run time", prog+" compose project")}
	case act.Value == "down" && o.has("v", "volumes"):
		return []Finding{extFinding(c, prog+"-compose-down", "removes the project's containers and its volumes, with their data", prog+" volume")}
	case act.Value == "down" || act.Value == "rm":
		return []Finding{extFinding(c, prog+"-compose-"+act.Value, "removes the project's containers", prog+" container")}
	}
	return nil
}

var kubeGlobals = []string{
	"namespace", "context", "cluster", "kubeconfig", "server", "user", "token", "as", "as-group", "as-uid",
	"request-timeout", "certificate-authority", "client-certificate", "client-key", "tls-server-name",
	"cache-dir", "log-file", "v", "password", "username", "profile", "profile-output",
}

func kubectl(c *parser.Command) []Finding {
	sub, rest, ok := subcommand(c.Args, "nsv", kubeGlobals...)
	if !ok || sub.Dynamic {
		return nil
	}
	o := parseOpts(rest, "nflo", append([]string{"filename", "selector", "output", "field-selector", "grace-period", "timeout"}, kubeGlobals...)...)
	if v, ok := o.value("dry-run"); ok && v != "none" && v != "false" {
		return nil
	}
	resource := "kubernetes " + c.Name + " resources"
	if len(o.operands) > 0 {
		var names []string
		for _, w := range o.operands {
			names = append(names, w.Value)
		}
		resource = "kubernetes " + strings.Join(names, " ")
	}
	switch sub.Value {
	case "delete":
		return []Finding{extFinding(c, "kubectl-delete", "deletes cluster resources", resource)}
	case "drain":
		return []Finding{extFinding(c, "kubectl-drain", "evicts every pod from a node", resource)}
	case "replace":
		if o.has("force") {
			return []Finding{extFinding(c, "kubectl-replace-force", "deletes and recreates cluster resources", resource)}
		}
	case "apply":
		if o.has("prune") {
			return []Finding{extFinding(c, "kubectl-apply-prune", "deletes cluster resources missing from the files", resource)}
		}
	}
	return nil
}

func helm(c *parser.Command) []Finding {
	sub, rest, ok := subcommand(c.Args, "n", "namespace", "kube-context", "kubeconfig", "registry-config", "repository-cache", "repository-config")
	if !ok || sub.Dynamic {
		return nil
	}
	switch sub.Value {
	case "uninstall", "delete", "del", "un":
		o := parseOpts(rest, "n", "namespace", "timeout", "cascade", "description")
		if o.has("dry-run") {
			return nil
		}
		return []Finding{extFinding(c, "helm-uninstall", "uninstalls a release", "helm release")}
	}
	return nil
}

var systemctlDestructive = setOf(
	"stop", "disable", "mask", "kill", "isolate", "poweroff", "reboot", "halt", "kexec", "soft-reboot",
	"emergency", "rescue", "suspend", "hibernate", "hybrid-sleep", "suspend-then-hibernate",
)

func systemctl(c *parser.Command) []Finding {
	o := parseOpts(c.Args, "HMtspnoP", "host", "machine", "type", "signal", "property", "state", "root", "lines",
		"output", "job-mode", "kill-whom", "kill-value", "what", "boot-loader-entry", "reboot-argument", "timestamp",
		"message", "image", "drop-in", "when", "kill-who", "image-policy")
	if len(o.operands) == 0 || o.has("help", "version") {
		return nil
	}
	verb := o.operands[0].Value
	if !systemctlDestructive[verb] {
		return nil
	}
	var units []string
	for _, w := range o.operands[1:] {
		units = append(units, w.Value)
	}
	resource := "systemd units " + strings.Join(units, " ")
	if len(units) == 0 {
		resource = "the machine"
	}
	return []Finding{extFinding(c, "systemctl-"+verb, "runs systemctl "+verb, strings.TrimSpace(resource))}
}

func service(c *parser.Command) []Finding {
	o := parseOpts(c.Args, "")
	if len(o.operands) < 2 {
		return nil
	}
	switch o.operands[1].Value {
	case "stop", "onestop", "forcestop", "disable", "onedisable":
		return []Finding{extFinding(c, "service-stop", "stops a system service", "service "+o.operands[0].Value)}
	}
	return nil
}

func launchctl(c *parser.Command) []Finding {
	o := parseOpts(c.Args, "")
	if len(o.operands) == 0 {
		return nil
	}
	switch o.operands[0].Value {
	case "unload", "remove", "bootout", "disable", "kill", "stop":
		return []Finding{extFinding(c, "launchctl-"+o.operands[0].Value, "stops or removes a launchd job", "launchd job")}
	}
	return nil
}

func crontab(c *parser.Command) []Finding {
	o := parseOpts(c.Args, "uT", "user")
	switch {
	case o.has("l", "e", "help", "version", "V", "T"):
		return nil
	case o.has("r"):
		return []Finding{extFinding(c, "crontab-remove", "removes every cron job of the user", "crontab")}
	case len(o.operands) > 0:
		return []Finding{extFinding(c, "crontab-replace", "replaces every cron job of the user", "crontab")}
	}
	return nil
}

// sqlText is one piece of SQL input.
type sqlText struct {
	text  string
	known bool
}

// stdinSQL is what a client reads on standard input, when a pipe, heredoc
// or redirection feeds it. Without one the client reads a terminal, which
// only a person types into.
func stdinSQL(c *parser.Command) (sqlText, bool) {
	if c.Stdin == nil {
		return sqlText{}, false
	}
	return sqlText{text: c.Stdin.Text, known: c.Stdin.Known}, true
}

const maxSQLFile = 4 << 20

// fileSQL reads a script a client was told to run.
func fileSQL(c *parser.Command, w parser.Word) sqlText {
	if w.Value == "-" && !w.Dynamic {
		t, _ := stdinSQL(c)
		return t
	}
	t := target(c, w)
	if t.Unresolved {
		return sqlText{}
	}
	fi, err := os.Stat(t.Path)
	if err != nil || fi.Size() > maxSQLFile {
		return sqlText{}
	}
	data, err := os.ReadFile(t.Path)
	if err != nil {
		return sqlText{}
	}
	return sqlText{text: string(data), known: true}
}

// sqlFindings turns client input into findings for a database server.
func sqlFindings(c *parser.Command, texts []sqlText, dialect, rule, resource string) []Finding {
	var out []Finding
	for _, t := range texts {
		if !t.known {
			out = append(out, extFinding(c, rule, "runs SQL that cannot be read beforehand", resource))
			continue
		}
		for _, r := range sqlRisks(t.text, dialect) {
			f := extFinding(c, rule, "runs "+r.what, resource)
			if r.unseen() {
				f.Rule = rule + "-unseen"
				f.Targets = []Target{unresolved("", r.what)}
			}
			out = append(out, f)
		}
	}
	return out
}

var uriCredentials = regexp.MustCompile(`://[^/@\s]*@`)

// hideCredentials drops the user and password part of a connection URI,
// so that it never reaches the log.
func hideCredentials(s string) string {
	return uriCredentials.ReplaceAllString(s, "://")
}

func psql(c *parser.Command) []Finding {
	o := parseOpts(c.Args, "cdfhpUvoLPFRT", "command", "dbname", "file", "host", "port", "username", "set", "variable",
		"output", "log-file", "pset", "field-separator", "record-separator", "table-attr")
	if o.has("help", "version", "V", "l", "list") {
		return nil
	}
	var texts []sqlText
	for _, w := range o.all("c", "command") {
		texts = append(texts, sqlText{text: w.Value, known: !w.Dynamic})
	}
	files := o.all("f", "file")
	for _, w := range files {
		texts = append(texts, fileSQL(c, w))
	}
	if len(texts) == 0 {
		if t, ok := stdinSQL(c); ok {
			texts = append(texts, t)
		}
	}
	db, ok := o.value("d", "dbname")
	if !ok && len(o.operands) > 0 {
		db = o.operands[0].Value
	}
	return sqlFindings(c, texts, dialectPostgres, "psql", strings.TrimSpace("postgres database "+hideCredentials(db)))
}

// withoutMySQLPassword drops -pSECRET style arguments, whose attached
// value would otherwise be read as more option letters.
func withoutMySQLPassword(args []parser.Word) []parser.Word {
	var out []parser.Word
	for _, a := range args {
		if strings.HasPrefix(a.Value, "-p") {
			continue
		}
		out = append(out, a)
	}
	return out
}

func mysql(c *parser.Command) []Finding {
	o := parseOpts(withoutMySQLPassword(c.Args), "DehPSu", "execute", "database", "host", "port", "socket", "user",
		"init-command", "default-character-set", "defaults-file", "defaults-extra-file", "login-path")
	if o.has("help", "version", "V") {
		return nil
	}
	var texts []sqlText
	for _, w := range o.all("e", "execute", "init-command") {
		texts = append(texts, sqlText{text: w.Value, known: !w.Dynamic})
	}
	if len(o.all("e", "execute")) == 0 {
		if t, ok := stdinSQL(c); ok {
			texts = append(texts, t)
		}
	}
	db, ok := o.value("D", "database")
	if !ok && len(o.operands) > 0 {
		db = o.operands[0].Value
	}
	return sqlFindings(c, texts, dialectMySQL, "mysql", strings.TrimSpace("mysql database "+hideCredentials(db)))
}

func mysqladmin(c *parser.Command) []Finding {
	o := parseOpts(withoutMySQLPassword(c.Args), "hPSu", "host", "port", "socket", "user", "defaults-file", "login-path")
	for i, w := range o.operands {
		switch strings.ToLower(w.Value) {
		case "drop":
			name := ""
			if i+1 < len(o.operands) {
				name = o.operands[i+1].Value
			}
			return []Finding{extFinding(c, "mysqladmin-drop", "drops a database", strings.TrimSpace("mysql database "+name))}
		case "shutdown", "kill":
			return []Finding{extFinding(c, "mysqladmin-"+strings.ToLower(w.Value), "stops the server or its connections", "mysql server")}
		}
	}
	return nil
}

var sqliteValueOpts = setOf(
	"cmd", "init", "separator", "newline", "nullvalue", "vfs", "mmap", "maxsize", "heap", "escape",
)

func sqlite(c *parser.Command) []Finding {
	var texts []sqlText
	var operands []parser.Word
	archive := false
	args := c.Args
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a.Dynamic || !strings.HasPrefix(a.Value, "-") || a.Value == "-" {
			operands = append(operands, a)
			continue
		}
		name := strings.TrimLeft(a.Value, "-")
		switch {
		case name == "help" || name == "version":
			return nil
		case name == "pagecache" || name == "lookaside":
			i += 2
		case strings.HasPrefix(name, "A") || name == "archive":
			archive = true
		case sqliteValueOpts[name] && i+1 < len(args):
			i++
			switch name {
			case "cmd":
				texts = append(texts, sqlText{text: args[i].Value, known: !args[i].Dynamic})
			case "init":
				texts = append(texts, fileSQL(c, args[i]))
			}
		}
	}
	var db Target
	haveDB := false
	if len(operands) > 0 {
		db = target(c, operands[0])
		db.Follow = true
		haveDB = true
		for _, w := range operands[1:] {
			texts = append(texts, sqlText{text: w.Value, known: !w.Dynamic})
		}
	}
	if len(operands) < 2 {
		if t, ok := stdinSQL(c); ok {
			texts = append(texts, t)
		}
	}
	var out []Finding
	if archive {
		out = append(out, Finding{Op: OpModify, Rule: "sqlite-archive", What: "works on files through an SQLite archive", Command: c,
			Targets: []Target{unresolved("", "archive mode reads and writes files")}})
	}
	// An in-memory database has no file to protect, but SQL that cannot
	// be read may still attach one.
	changes := ""
	unknown := false
	for _, t := range texts {
		if !t.known {
			unknown = true
			continue
		}
		for _, r := range sqlRisks(t.text, dialectSQLite) {
			if r.unseen() {
				out = append(out, Finding{Op: OpModify, Rule: "sqlite-unseen", What: "runs " + r.what, Command: c,
					Targets: []Target{unresolved("", r.what)}})
				continue
			}
			if changes == "" {
				changes = "runs " + r.what
			}
		}
	}
	switch {
	case unknown:
		f := Finding{Op: OpModify, Rule: "sqlite", What: "runs SQL that cannot be read beforehand", Command: c,
			Targets: []Target{unresolved("", "the SQL is only known at run time")}}
		if haveDB {
			f.Targets = append(f.Targets, db)
		}
		out = append(out, f)
	case changes != "" && haveDB:
		out = append(out, Finding{Op: OpModify, Rule: "sqlite", What: changes, Command: c, Targets: []Target{db}})
	}
	return out
}

// redisDangerous lists commands that drop data, stop the server, or run a
// script that could do either.
var redisDangerous = setOf("FLUSHALL", "FLUSHDB", "SHUTDOWN", "SWAPDB", "EVAL", "EVALSHA", "FCALL")

func redis(c *parser.Command) []Finding {
	o := parseOpts(c.Args, "hpsaurindD", "user", "pass", "sni", "cacert", "cacertdir", "cert", "key", "tls-ciphers",
		"tls-ciphersuites", "lru-test", "rdb", "functions-rdb", "pipe-timeout", "memkeys-samples", "pattern", "count",
		"quoted-pattern", "intrinsic-latency", "eval", "cluster")
	if o.has("help", "version") {
		return nil
	}
	if w, ok := o.word("eval"); ok {
		return []Finding{{Op: OpExternal, Rule: "redis-eval", What: "runs a Lua script on the server", Command: c, Resource: "redis",
			Targets: []Target{unresolved(w.Value, "the script can change any data")}}}
	}
	var lines []string
	if len(o.operands) > 0 {
		if o.operands[0].Dynamic {
			return []Finding{extFinding(c, "redis", "runs a command only known at run time", "redis")}
		}
		var parts []string
		for _, w := range o.operands {
			parts = append(parts, w.Value)
		}
		lines = []string{strings.Join(parts, " ")}
	} else if t, ok := stdinSQL(c); ok {
		if !t.known {
			return []Finding{extFinding(c, "redis", "runs commands that cannot be read beforehand", "redis")}
		}
		lines = strings.Split(t.text, "\n")
	}
	for _, line := range lines {
		fields := strings.Fields(strings.ToUpper(line))
		if len(fields) == 0 {
			continue
		}
		if redisDangerous[fields[0]] || (len(fields) > 1 && (fields[0] == "SCRIPT" || fields[0] == "FUNCTION") && fields[1] == "FLUSH") {
			return []Finding{extFinding(c, "redis-"+strings.ToLower(fields[0]), "runs "+fields[0], "redis")}
		}
	}
	return nil
}

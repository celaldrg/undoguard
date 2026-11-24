package detect

import (
	"regexp"
	"strings"
)

// Dialects of SQL client input.
const (
	dialectPostgres = "postgres"
	dialectMySQL    = "mysql"
	dialectSQLite   = "sqlite"
)

// sqlRisk is one dangerous thing found in SQL client input.
type sqlRisk struct {
	what string
	// shell is set when the client would run a shell command.
	shell bool
	// attach is set when SQLite would open another database file.
	attach bool
	// opaque is set when the client would run SQL that is not in the
	// input itself, such as another script file.
	opaque bool
}

// unseen reports whether the risk hides what will run or where.
func (r sqlRisk) unseen() bool {
	return r.shell || r.attach || r.opaque
}

var (
	dollarTag    = regexp.MustCompile(`^\$[A-Za-z_]*\$`)
	sqlWord      = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_$]*|[()]`)
	psqlShell    = regexp.MustCompile(`(?i)\\!|\\(o|g|gx|w)\s*\||\\copy\b.*\bprogram\b`)
	psqlInclude  = regexp.MustCompile(`(?i)\\(i|ir|include|include_relative|gexec)\b`)
	mysqlShell   = regexp.MustCompile(`(?im)^\s*(\\!|system\s)`)
	mysqlInclude = regexp.MustCompile(`(?im)^\s*(\\\.|source\s)`)
	copyProgram  = regexp.MustCompile(`(?is)\bcopy\b.*\b(to|from)\s+program\b`)
)

// sqlRisks scans client input for destructive statements and for client
// commands that run a shell.
func sqlRisks(text, dialect string) []sqlRisk {
	var out []sqlRisk
	switch dialect {
	case dialectPostgres:
		if psqlShell.MatchString(text) {
			out = append(out, sqlRisk{what: "a psql command that runs a shell command", shell: true})
		}
		if psqlInclude.MatchString(text) {
			out = append(out, sqlRisk{what: "a psql command that runs SQL from elsewhere", opaque: true})
		}
	case dialectMySQL:
		if mysqlShell.MatchString(text) {
			out = append(out, sqlRisk{what: "a mysql client command that runs a shell command", shell: true})
		}
		if mysqlInclude.MatchString(text) {
			out = append(out, sqlRisk{what: "a mysql client command that runs SQL from another file", opaque: true})
		}
	case dialectSQLite:
		var sql []string
		for _, line := range strings.Split(text, "\n") {
			t := strings.TrimSpace(line)
			if !strings.HasPrefix(t, ".") {
				sql = append(sql, line)
				continue
			}
			if r, ok := sqliteDot(t); ok {
				out = append(out, r)
			}
		}
		text = strings.Join(sql, "\n")
	}
	clean := stripSQL(text, dialect == dialectMySQL)
	if copyProgram.MatchString(clean) {
		out = append(out, sqlRisk{what: "COPY ... PROGRAM, which runs a shell command on the server", shell: true})
	}
	for _, stmt := range strings.Split(clean, ";") {
		if r, ok := stmtRisk(stmt); ok {
			out = append(out, r)
		}
	}
	return out
}

// sqliteDot checks one sqlite3 dot-command.
func sqliteDot(line string) (sqlRisk, bool) {
	name, rest, _ := strings.Cut(line, " ")
	rest = strings.TrimLeft(strings.TrimSpace(rest), `'"`)
	switch strings.ToLower(name) {
	case ".shell", ".system":
		return sqlRisk{what: "the sqlite3 " + name + " command, which runs a shell command", shell: true}, true
	case ".once", ".output", ".read", ".import", ".excel":
		if strings.HasPrefix(rest, "|") {
			return sqlRisk{what: "a sqlite3 command that pipes through a shell command", shell: true}, true
		}
		if strings.ToLower(name) == ".read" {
			return sqlRisk{what: "the sqlite3 .read command, which runs SQL from another file", opaque: true}, true
		}
	case ".open", ".restore", ".backup", ".save", ".clone":
		return sqlRisk{what: "the sqlite3 " + name + " command, which works on another database file", attach: true}, true
	}
	return sqlRisk{}, false
}

// stripSQL blanks out comments and quoted text so that keywords inside
// them are not mistaken for statements.
func stripSQL(s string, mysql bool) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		ch := s[i]
		switch {
		case ch == '-' && i+1 < len(s) && s[i+1] == '-', ch == '#' && mysql:
			j := strings.IndexByte(s[i:], '\n')
			if j < 0 {
				i = len(s)
			} else {
				i += j
			}
			b.WriteByte(' ')
		case ch == '/' && i+1 < len(s) && s[i+1] == '*':
			j := strings.Index(s[i+2:], "*/")
			if j < 0 {
				i = len(s)
			} else {
				i += j + 4
			}
			b.WriteByte(' ')
		case ch == '\'' || ch == '"' || ch == '`':
			q := ch
			i++
			for i < len(s) {
				if s[i] == '\\' && mysql && i+1 < len(s) {
					i += 2
					continue
				}
				if s[i] == q {
					if i+1 < len(s) && s[i+1] == q {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			b.WriteString(" x ")
		case ch == '$' && !mysql && dollarTag.MatchString(s[i:]):
			tag := dollarTag.FindString(s[i:])
			end := strings.Index(s[i+len(tag):], tag)
			if end < 0 {
				i = len(s)
			} else {
				i += len(tag) + end + len(tag)
			}
			b.WriteString(" x ")
		default:
			b.WriteByte(ch)
			i++
		}
	}
	return b.String()
}

// stmtRisk looks at one statement's top-level keywords.
func stmtRisk(stmt string) (sqlRisk, bool) {
	depth := 0
	var top []string
	for _, t := range sqlWord.FindAllString(stmt, -1) {
		switch t {
		case "(":
			depth++
		case ")":
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 {
				top = append(top, strings.ToUpper(t))
			}
		}
	}
	if len(top) == 0 {
		return sqlRisk{}, false
	}
	index := func(from int, w string) int {
		for i := from; i < len(top); i++ {
			if top[i] == w {
				return i
			}
		}
		return -1
	}
	lead := top[0]
	switch {
	case lead == "DROP":
		end := min(len(top), 3)
		return sqlRisk{what: strings.Join(top[:end], " ")}, true
	case lead == "TRUNCATE":
		return sqlRisk{what: "TRUNCATE"}, true
	case lead == "ALTER" && index(1, "DROP") > 0:
		return sqlRisk{what: "ALTER ... DROP"}, true
	case lead == "ATTACH":
		return sqlRisk{what: "ATTACH, which opens another database file", attach: true}, true
	}
	if lead == "DELETE" || lead == "WITH" {
		if i := index(0, "DELETE"); i >= 0 && index(i, "WHERE") < 0 {
			return sqlRisk{what: "DELETE without WHERE"}, true
		}
	}
	if lead == "UPDATE" || lead == "WITH" {
		if i := index(0, "UPDATE"); i >= 0 && index(i, "SET") > i && index(i, "WHERE") < 0 {
			return sqlRisk{what: "UPDATE without WHERE"}, true
		}
	}
	return sqlRisk{}, false
}

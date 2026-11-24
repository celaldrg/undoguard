package parser

import (
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"mvdan.cc/sh/v3/syntax"
)

// Words are built as "patterns": text in which every character that came
// from a quoted context and could be special to brace expansion or
// globbing is preceded by a backslash. Unquoted metacharacters stay bare,
// so a single string carries both the text and which parts are live.

const patSpecial = "\\*?[]{},"

// escAll escapes every special character, for quoted text.
func escAll(s string) string {
	if !strings.ContainsAny(s, patSpecial) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if strings.IndexByte(patSpecial, s[i]) >= 0 {
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// escNoGlob escapes brace characters and backslashes but leaves glob
// metacharacters live. It is used for unquoted expansion results, which
// are globbed but never brace expanded.
func escNoGlob(s string) string {
	if !strings.ContainsAny(s, "\\{},") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if strings.IndexByte("\\{},", s[i]) >= 0 {
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// unescape turns a pattern back into the literal text.
func unescape(p string) string {
	if !strings.Contains(p, "\\") {
		return p
	}
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		if p[i] == '\\' && i+1 < len(p) {
			i++
		}
		b.WriteByte(p[i])
	}
	return b.String()
}

// hasMeta reports whether p contains a live glob metacharacter.
func hasMeta(p string) bool {
	for i := 0; i < len(p); i++ {
		switch p[i] {
		case '\\':
			i++
		case '*', '?', '[':
			return true
		}
	}
	return false
}

// braceExpand expands {a,b} and {1..3} forms whose braces are live. It
// returns false when the result would exceed maxBraceItems.
func braceExpand(p string) ([]string, bool) {
	out := []string{}
	ok := braceInto(p, &out, 0)
	return out, ok
}

const maxBraceItems = 1024

func braceInto(p string, out *[]string, depth int) bool {
	if depth > 32 {
		*out = append(*out, p)
		return true
	}
	for i := 0; i < len(p); i++ {
		if p[i] == '\\' {
			i++
			continue
		}
		if p[i] != '{' {
			continue
		}
		closeAt, commas := -1, []int{}
		level := 0
		for j := i; j < len(p); j++ {
			if p[j] == '\\' {
				j++
				continue
			}
			switch p[j] {
			case '{':
				level++
			case '}':
				level--
			case ',':
				if level == 1 {
					commas = append(commas, j)
				}
			}
			if level == 0 {
				closeAt = j
				break
			}
		}
		if closeAt < 0 {
			continue
		}
		var alts []string
		if len(commas) > 0 {
			start := i + 1
			for _, c := range commas {
				alts = append(alts, p[start:c])
				start = c + 1
			}
			alts = append(alts, p[start:closeAt])
		} else if seq, ok, overflow := braceSeq(p[i+1 : closeAt]); overflow {
			return false
		} else if ok {
			alts = seq
		} else {
			continue
		}
		pre, post := p[:i], p[closeAt+1:]
		for _, a := range alts {
			if len(*out) > maxBraceItems {
				return false
			}
			if !braceInto(pre+a+post, out, depth+1) {
				return false
			}
		}
		return len(*out) <= maxBraceItems
	}
	*out = append(*out, p)
	return len(*out) <= maxBraceItems
}

// braceSeq handles {a..b} and {a..b..step} for integers and single
// letters. overflow is set when the sequence is too long to list.
func braceSeq(body string) (items []string, ok bool, overflow bool) {
	parts := strings.Split(body, "..")
	if len(parts) != 2 && len(parts) != 3 {
		return nil, false, false
	}
	step := 1
	if len(parts) == 3 {
		n, err := strconv.Atoi(parts[2])
		if err != nil || n == 0 {
			return nil, false, false
		}
		if n < 0 {
			n = -n
		}
		step = n
	}
	a, errA := strconv.Atoi(parts[0])
	b, errB := strconv.Atoi(parts[1])
	if errA == nil && errB == nil {
		if (b-a)/step > maxBraceItems || (a-b)/step > maxBraceItems {
			return nil, false, true
		}
		var out []string
		if a <= b {
			for i := a; i <= b; i += step {
				out = append(out, strconv.Itoa(i))
			}
		} else {
			for i := a; i >= b; i -= step {
				out = append(out, strconv.Itoa(i))
			}
		}
		return out, true, false
	}
	if len(parts[0]) == 1 && len(parts[1]) == 1 && isLetter(parts[0][0]) && isLetter(parts[1][0]) {
		x, y := parts[0][0], parts[1][0]
		var out []string
		if x <= y {
			for c := int(x); c <= int(y); c += step {
				out = append(out, string(rune(c)))
			}
		} else {
			for c := int(x); c >= int(y); c -= step {
				out = append(out, string(rune(c)))
			}
		}
		return out, true, false
	}
	return nil, false, false
}

func isLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// shellMatch matches s against a pattern in which "*" also matches "/",
// as in parameter expansion and case statements.
func shellMatch(p, s string) bool {
	for len(p) > 0 {
		switch p[0] {
		case '*':
			for len(p) > 0 && p[0] == '*' {
				p = p[1:]
			}
			if p == "" {
				return true
			}
			for i := 0; i <= len(s); i++ {
				if shellMatch(p, s[i:]) {
					return true
				}
			}
			return false
		case '?':
			if s == "" {
				return false
			}
			_, n := utf8.DecodeRuneInString(s)
			p, s = p[1:], s[n:]
		case '[':
			if s == "" {
				return false
			}
			end := strings.IndexByte(p[1:], ']')
			if end < 0 {
				if s[0] != '[' {
					return false
				}
				p, s = p[1:], s[1:]
				continue
			}
			class := p[1 : end+1]
			r, n := utf8.DecodeRuneInString(s)
			if !classMatch(class, r) {
				return false
			}
			p, s = p[end+2:], s[n:]
		case '\\':
			if len(p) < 2 || s == "" || s[0] != p[1] {
				return false
			}
			p, s = p[2:], s[1:]
		default:
			if s == "" || s[0] != p[0] {
				return false
			}
			p, s = p[1:], s[1:]
		}
	}
	return s == ""
}

func classMatch(class string, r rune) bool {
	negate := false
	if strings.HasPrefix(class, "!") || strings.HasPrefix(class, "^") {
		negate, class = true, class[1:]
	}
	matched := false
	for i := 0; i < len(class); {
		lo, n := utf8.DecodeRuneInString(class[i:])
		i += n
		hi := lo
		if i+1 < len(class) && class[i] == '-' {
			h, m := utf8.DecodeRuneInString(class[i+1:])
			hi = h
			i += 1 + m
		}
		if r >= lo && r <= hi {
			matched = true
		}
	}
	return matched != negate
}

// removeAffix implements ${x#p}, ${x##p}, ${x%p} and ${x%%p}.
func removeAffix(op syntax.ParExpOperator, val, pat string) string {
	switch op {
	case syntax.RemSmallPrefix:
		for i := 0; i <= len(val); i++ {
			if shellMatch(pat, val[:i]) {
				return val[i:]
			}
		}
	case syntax.RemLargePrefix:
		for i := len(val); i >= 0; i-- {
			if shellMatch(pat, val[:i]) {
				return val[i:]
			}
		}
	case syntax.RemSmallSuffix:
		for i := len(val); i >= 0; i-- {
			if shellMatch(pat, val[i:]) {
				return val[:i]
			}
		}
	case syntax.RemLargeSuffix:
		for i := 0; i <= len(val); i++ {
			if shellMatch(pat, val[i:]) {
				return val[:i]
			}
		}
	}
	return val
}

// ansiC decodes the body of a $'...' string.
func ansiC(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' || i+1 >= len(s) {
			b.WriteByte(c)
			continue
		}
		i++
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case 'a':
			b.WriteByte(7)
		case 'b':
			b.WriteByte(8)
		case 'e', 'E':
			b.WriteByte(27)
		case 'f':
			b.WriteByte(12)
		case 'v':
			b.WriteByte(11)
		case '\\', '\'', '"', '?':
			b.WriteByte(s[i])
		case 'c':
			if i+1 < len(s) {
				i++
				b.WriteByte(s[i] & 0x1f)
			}
		case 'x':
			j := i + 1
			for j < len(s) && j < i+3 && isHex(s[j]) {
				j++
			}
			if j == i+1 {
				b.WriteString(`\x`)
				continue
			}
			n, _ := strconv.ParseUint(s[i+1:j], 16, 8)
			b.WriteByte(byte(n))
			i = j - 1
		case 'u', 'U':
			limit := 4
			if s[i] == 'U' {
				limit = 8
			}
			j := i + 1
			for j < len(s) && j < i+1+limit && isHex(s[j]) {
				j++
			}
			if j == i+1 {
				b.WriteByte('\\')
				b.WriteByte(s[i])
				continue
			}
			n, _ := strconv.ParseUint(s[i+1:j], 16, 32)
			b.WriteRune(rune(n))
			i = j - 1
		default:
			if s[i] >= '0' && s[i] <= '7' {
				j := i
				for j < len(s) && j < i+3 && s[j] >= '0' && s[j] <= '7' {
					j++
				}
				n, _ := strconv.ParseUint(s[i:j], 8, 16)
				b.WriteByte(byte(n))
				i = j - 1
				continue
			}
			b.WriteByte('\\')
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// dqUnescape applies the backslash rules of double quotes.
func dqUnescape(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case '$', '`', '"', '\\':
				i++
				b.WriteByte(s[i])
				continue
			case '\n':
				i++
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// dropHidden removes matches that bash would not produce because a
// wildcard at the start of a path element does not match a leading dot.
func dropHidden(pattern string, matches []string) []string {
	pc := strings.Split(pattern, "/")
	out := matches[:0]
	for _, m := range matches {
		mc := strings.Split(m, "/")
		keep := true
		if len(mc) == len(pc) {
			for i := range pc {
				if pc[i] != "" && strings.IndexByte("*?[", pc[i][0]) >= 0 && strings.HasPrefix(mc[i], ".") {
					keep = false
					break
				}
			}
		}
		if keep {
			out = append(out, m)
		}
	}
	return out
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

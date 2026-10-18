// Package mdconv converts between the local Obsidian markdown and the
// markdown that Google Docs imports and exports.
//
// What Google Docs does to markdown (verified against the real API, see
// CLAUDE.md "Google Docs markdown behaviour"):
//   - Import joins the lines of a paragraph into one line unless a line ends
//     with a hard break (two spaces or a backslash).
//   - Export escapes almost every ASCII punctuation character with a
//     backslash (\[\[link\]\], \#tag, 18\. 10\., C:\\Users), adds two spaces
//     at the end of list items, puts whitespace-only lines between some list
//     items and writes table separators as | :---- |.
//   - Code fences are dropped on import; the code lines become paragraphs.
//   - Export is a fixed point: export → import → export gives the same text.
//
// The Google Doc itself never shows those backslashes; they only exist in the
// export. Clean removes them so they never reach the Obsidian file.
package mdconv

import (
	"regexp"
	"strings"
)

// NormalizeNewlines turns CRLF and CR into LF and drops a UTF-8 BOM.
func NormalizeNewlines(s string) string {
	s = strings.TrimPrefix(s, "\ufeff")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

var (
	listItemRe = regexp.MustCompile(`^\s*([-*+]|\d+[.)])\s`)
	tableSepRe = regexp.MustCompile(`^\s*\|?\s*:?-{3,}:?\s*(\|\s*:?-{3,}:?\s*)*\|?\s*$`)
	sameLinkRe = regexp.MustCompile(`\[([^\]\n]+)\]\(([^)\s]+)\)`)
	fenceRe    = regexp.MustCompile("^\\s*(```|~~~)")
	headingRe  = regexp.MustCompile(`^#{1,6}(\s|$)`)
	boldHeadRe = regexp.MustCompile(`^(#{1,6} +)\*\*([^*]+)\*\*\s*$`)
	hrRe       = regexp.MustCompile(`^\s*([-*_])(\s*[-*_]){2,}\s*$`)
	quoteRe    = regexp.MustCompile(`^\s*>`)
	tableRowRe = regexp.MustCompile(`^\s*\|.*\|\s*$`)
)

// IsListItem reports whether the line starts a list item.
func IsListItem(line string) bool { return listItemRe.MatchString(line) }

// Clean turns a Google Docs markdown export into the text that goes into the
// Obsidian file: no export-only backslashes, no trailing spaces, no blank
// lines inside lists, simple table separators, bare URLs instead of
// [url](url) links, LF line endings and exactly one newline at the end.
func Clean(export string) string {
	lines := strings.Split(NormalizeNewlines(export), "\n")
	var out []string
	inCode := false
	for _, l := range lines {
		l = strings.TrimRight(l, " \t\u00a0")
		if fenceRe.MatchString(l) {
			inCode = !inCode
			out = append(out, l)
			continue
		}
		if inCode {
			out = append(out, l)
			continue
		}
		if tableSepRe.MatchString(l) && strings.Contains(l, "|") {
			l = simpleTableSep(l)
		} else {
			l = Unescape(l)
			l = bareLinks(l)
			// Docs sometimes exports a heading as "# **Title**" (seen on
			// documents created through the API); a heading is bold anyway.
			l = boldHeadRe.ReplaceAllString(l, "$1$2")
		}
		out = append(out, l)
	}
	// Drop blank lines that sit between two list items.
	var res []string
	for i := 0; i < len(out); i++ {
		if out[i] == "" && len(res) > 0 && IsListItem(res[len(res)-1]) {
			j := i
			for j < len(out) && out[j] == "" {
				j++
			}
			if j < len(out) && IsListItem(out[j]) {
				i = j - 1
				continue
			}
		}
		// Collapse runs of blank lines.
		if out[i] == "" && len(res) > 0 && res[len(res)-1] == "" {
			continue
		}
		res = append(res, out[i])
	}
	for len(res) > 0 && res[0] == "" {
		res = res[1:]
	}
	for len(res) > 0 && res[len(res)-1] == "" {
		res = res[:len(res)-1]
	}
	if len(res) == 0 {
		return ""
	}
	return strings.Join(res, "\n") + "\n"
}

func simpleTableSep(l string) string {
	cells := strings.Split(strings.Trim(strings.TrimSpace(l), "|"), "|")
	for i, c := range cells {
		c = strings.TrimSpace(c)
		left, right := strings.HasPrefix(c, ":"), strings.HasSuffix(c, ":")
		switch {
		case left && right:
			cells[i] = " :---: "
		case right:
			cells[i] = " ---: "
		default:
			// Docs writes :---- for every plain column (left is the default).
			cells[i] = " --- "
		}
	}
	return "|" + strings.Join(cells, "|") + "|"
}

// bareLinks turns [https://x](https://x) back into https://x: Docs links bare
// URLs automatically and the export writes them out as full links.
func bareLinks(l string) string {
	return sameLinkRe.ReplaceAllStringFunc(l, func(m string) string {
		sub := sameLinkRe.FindStringSubmatch(m)
		if sub[1] == sub[2] || strings.TrimPrefix(sub[1], "mailto:") == strings.TrimPrefix(sub[2], "mailto:") {
			return sub[2]
		}
		return m
	})
}

// keepEscaped lists the characters whose backslash escape is kept, because
// without it Obsidian would render the text differently from the Google Doc
// (2\*3\*4 would turn italic, \` would open inline code).
const keepEscaped = "*`"

// Unescape removes the backslash escapes the Docs export adds. It leaves code
// spans alone, keeps escapes that change meaning in Obsidian (see
// keepEscaped) and keeps an escape at the start of a line when removing it
// would turn the line into a heading, list item, quote or numbered list.
func Unescape(l string) string {
	if !strings.Contains(l, `\`) {
		return l
	}
	var b strings.Builder
	inCode := false
	atStart := true     // only whitespace so far
	digitsOnly := false // only whitespace and then digits so far
	for i := 0; i < len(l); i++ {
		c := l[i]
		if c == '`' {
			inCode = !inCode
			b.WriteByte(c)
			atStart, digitsOnly = false, false
			continue
		}
		if c == '\\' && !inCode && i+1 < len(l) && isASCIIPunct(l[i+1]) {
			n, rest := l[i+1], l[i+2:]
			keep := strings.IndexByte(keepEscaped, n) >= 0
			if n == '\\' {
				// \\ is a literal backslash. Keep the pair only when the next
				// character would otherwise be escaped by it.
				keep = rest != "" && isASCIIPunct(rest[0])
			}
			if atStart && structuralAtStart(n, rest) {
				keep = true
			}
			// "18\. 10" at the start of a line would become a numbered list.
			if digitsOnly && (n == '.' || n == ')') && (rest == "" || rest[0] == ' ') {
				keep = true
			}
			if keep {
				b.WriteByte(c)
			}
			b.WriteByte(n)
			i++
			atStart, digitsOnly = false, false
			continue
		}
		switch {
		case c == ' ' || c == '\t':
			if digitsOnly {
				digitsOnly = false
			}
		case c >= '0' && c <= '9' && (atStart || digitsOnly):
			atStart, digitsOnly = false, true
		default:
			atStart, digitsOnly = false, false
		}
		b.WriteByte(c)
	}
	return b.String()
}

// structuralAtStart reports whether char n at the start of a line, followed
// by rest, would start a block (heading, list item, quote).
func structuralAtStart(n byte, rest string) bool {
	switch n {
	case '#':
		return rest == "" || rest[0] == ' ' || rest[0] == '#'
	case '-', '+':
		return rest == "" || rest[0] == ' '
	case '>':
		return true
	case '=':
		return strings.Trim(rest, "=") == ""
	}
	return false
}

func isASCIIPunct(c byte) bool {
	return (c >= '!' && c <= '/') || (c >= ':' && c <= '@') || (c >= '[' && c <= '`') || (c >= '{' && c <= '~')
}

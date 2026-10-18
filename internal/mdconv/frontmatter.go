package mdconv

import (
	"regexp"
	"strings"
)

// Front matter rule (Simon, 18 Oct 2026): in the Google Doc, front matter is
// everything between a horizontal line at the very start of the document and
// the next horizontal line. Each key is its own paragraph; list values may be
// bullets under the key or plain text ("tags: a, b"). Locally it is ordinary
// YAML between --- lines. Only the first block at the very start counts;
// every other horizontal line is plain content.

// FMEntry is one top-level front matter entry.
type FMEntry struct {
	Key   string   // empty for a line kept verbatim (Raw)
	Value string   // scalar value, or the flow list text "[a, b]"
	Items []string // list items when List
	List  bool
	Flow  bool     // written as [a, b]
	Raw   string   // unparsed line (comment, nested YAML), kept verbatim
	Lines []string // the original local lines of this entry
}

var (
	fmKeyRe  = regexp.MustCompile(`^([A-Za-z0-9_][^:]*?):(\s+(.*))?$`)
	fmItemRe = regexp.MustCompile(`^(\s*)-\s+(.*)$`)
)

// listKeys are keys whose plain-text value is split into a list when it comes
// from the Google Doc ("tags: a, b" → a list), as Obsidian expects lists there.
var listKeys = map[string]bool{"tags": true, "tag": true, "aliases": true, "alias": true, "cssclasses": true, "cssclass": true}

// SplitLocal splits a local file into its front matter lines (without the
// --- fences) and the body. ok is false when the file has no front matter.
func SplitLocal(text string) (fm []string, body string, ok bool) {
	if !strings.HasPrefix(text, "---\n") {
		return nil, text, false
	}
	lines := strings.Split(text, "\n")
	for i := 1; i < len(lines); i++ {
		if lines[i] == "---" || lines[i] == "..." {
			return lines[1:i], strings.Join(lines[i+1:], "\n"), true
		}
	}
	return nil, text, false
}

// SplitExport splits a cleaned Docs export into its front matter lines and the
// body. The export writes the horizontal lines as "---".
func SplitExport(clean string) (fm []string, body string, ok bool) {
	lines := strings.Split(clean, "\n")
	if len(lines) == 0 || !hrRe.MatchString(lines[0]) {
		return nil, clean, false
	}
	for i := 1; i < len(lines); i++ {
		if hrRe.MatchString(lines[i]) {
			body := strings.Join(lines[i+1:], "\n")
			return lines[1:i], strings.TrimLeft(body, "\n"), true
		}
	}
	return nil, clean, false
}

// ParseLocalFM parses local YAML front matter lines into entries.
func ParseLocalFM(lines []string) []FMEntry {
	var out []FMEntry
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		m := fmKeyRe.FindStringSubmatch(l)
		if m == nil {
			out = append(out, FMEntry{Raw: l, Lines: []string{l}})
			continue
		}
		e := FMEntry{Key: m[1], Value: strings.TrimSpace(m[3]), Lines: []string{l}}
		if e.Value == "" {
			for i+1 < len(lines) {
				im := fmItemRe.FindStringSubmatch(lines[i+1])
				if im == nil {
					break
				}
				e.List = true
				e.Items = append(e.Items, unquote(strings.TrimSpace(im[2])))
				e.Lines = append(e.Lines, lines[i+1])
				i++
			}
		} else if strings.HasPrefix(e.Value, "[") && strings.HasSuffix(e.Value, "]") {
			e.List, e.Flow = true, true
			e.Items = splitList(strings.TrimSuffix(strings.TrimPrefix(e.Value, "["), "]"))
		}
		out = append(out, e)
	}
	return out
}

// ParseExportFM parses the front matter lines of a cleaned Docs export.
func ParseExportFM(lines []string) []FMEntry {
	var out []FMEntry
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" {
			continue
		}
		if im := fmItemRe.FindStringSubmatch(l); im != nil && len(out) > 0 && out[len(out)-1].Key != "" && (out[len(out)-1].List || out[len(out)-1].Value == "") {
			last := &out[len(out)-1]
			last.List, last.Flow = true, false
			last.Items = append(last.Items, cleanItem(last.Key, im[2]))
			continue
		}
		m := fmKeyRe.FindStringSubmatch(t)
		if m == nil {
			out = append(out, FMEntry{Raw: t})
			continue
		}
		e := FMEntry{Key: m[1], Value: strings.TrimSpace(m[3])}
		switch {
		case strings.HasPrefix(e.Value, "[") && strings.HasSuffix(e.Value, "]"):
			e.List, e.Flow = true, true
			e.Items = splitList(strings.TrimSuffix(strings.TrimPrefix(e.Value, "["), "]"))
		case e.Value != "" && listKeys[strings.ToLower(e.Key)]:
			e.List = true
			sep := ","
			if !strings.Contains(e.Value, ",") {
				sep = " "
			}
			for _, it := range strings.Split(e.Value, sep) {
				if it = cleanItem(e.Key, it); it != "" {
					e.Items = append(e.Items, it)
				}
			}
			e.Value = ""
		}
		out = append(out, e)
	}
	return out
}

func cleanItem(key, it string) string {
	it = unquote(strings.TrimSpace(it))
	if strings.HasPrefix(strings.ToLower(key), "tag") {
		it = strings.TrimPrefix(it, "#")
	}
	return it
}

func splitList(s string) []string {
	var out []string
	for _, it := range strings.Split(s, ",") {
		if it = unquote(strings.TrimSpace(it)); it != "" {
			out = append(out, it)
		}
	}
	return out
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' && s[len(s)-1] == '"' || s[0] == '\'' && s[len(s)-1] == '\'') {
		return s[1 : len(s)-1]
	}
	return s
}

// EqualFM compares two parsed front matters by meaning, not by formatting.
func EqualFM(a, b []FMEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !equalEntry(a[i], b[i]) {
			return false
		}
	}
	return true
}

func equalEntry(a, b FMEntry) bool {
	if a.Key != b.Key || strings.TrimSpace(a.Raw) != strings.TrimSpace(b.Raw) {
		return false
	}
	if a.List != b.List {
		// "tags:" with no items equals an empty list.
		return len(a.Items) == 0 && len(b.Items) == 0 && a.Value == "" && b.Value == ""
	}
	if a.List {
		if len(a.Items) != len(b.Items) {
			return false
		}
		for i := range a.Items {
			if a.Items[i] != b.Items[i] {
				return false
			}
		}
		return true
	}
	return unquote(a.Value) == unquote(b.Value)
}

// RenderDocFM renders front matter for the Google Doc: a horizontal line,
// one paragraph per key, list items as bullets, a closing horizontal line.
func RenderDocFM(entries []FMEntry) string {
	var b strings.Builder
	b.WriteString("***\n\n")
	for _, e := range entries {
		switch {
		case e.Key == "":
			raw := strings.TrimSpace(e.Raw)
			if raw == "" {
				continue
			}
			if strings.HasPrefix(raw, "#") || strings.HasPrefix(raw, "-") {
				raw = `\` + raw
			}
			b.WriteString(raw + "\n\n")
		case e.List && !e.Flow:
			b.WriteString(e.Key + ":\n\n")
			for _, it := range e.Items {
				b.WriteString("- " + it + "\n")
			}
			b.WriteString("\n")
		default:
			b.WriteString(e.Key + ": " + e.Value + "\n\n")
		}
	}
	b.WriteString("***\n\n")
	return b.String()
}

// RenderLocalFM renders entries as YAML lines. Entries that mean the same as
// an entry of prev keep prev's exact lines, so untouched keys come back
// byte for byte; changed entries use the list indentation prev used.
func RenderLocalFM(entries []FMEntry, prev []FMEntry) []string {
	indent := "  - "
	for _, p := range prev {
		if p.List && !p.Flow && len(p.Lines) > 1 {
			if m := fmItemRe.FindStringSubmatch(p.Lines[1]); m != nil {
				indent = m[1] + "- "
			}
			break
		}
	}
	used := make([]bool, len(prev))
	var out []string
	for _, e := range entries {
		reused := false
		for i, p := range prev {
			if !used[i] && equalEntry(e, p) {
				out = append(out, p.Lines...)
				used[i], reused = true, true
				break
			}
		}
		if reused {
			continue
		}
		switch {
		case e.Key == "":
			out = append(out, e.Raw)
		case e.List && e.Flow:
			q := make([]string, len(e.Items))
			for i, it := range e.Items {
				q[i] = yamlScalar(it)
			}
			out = append(out, e.Key+": ["+strings.Join(q, ", ")+"]")
		case e.List:
			out = append(out, e.Key+":")
			for _, it := range e.Items {
				out = append(out, indent+yamlScalar(it))
			}
		case e.Value == "":
			out = append(out, e.Key+":")
		default:
			out = append(out, e.Key+": "+e.Value)
		}
	}
	return out
}

func yamlScalar(s string) string {
	if s == "" || strings.ContainsAny(s[:1], "[]{}#&*!|>'\"%@`,") || strings.Contains(s, ": ") || strings.Contains(s, " #") {
		return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
	}
	return s
}

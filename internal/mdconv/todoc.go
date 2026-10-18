package mdconv

import "strings"

// ToDoc turns a local Obsidian file into the markdown that is uploaded to the
// Google Doc. The upload replaces the whole document; Docs converts the
// markdown into real headings, lists, tables and checkboxes.
//
//   - Front matter becomes the horizontal-line block (see frontmatter.go).
//   - Lines of one paragraph get a hard break, so the Doc shows them on
//     separate lines like Obsidian does instead of joining them.
//   - Everything else is passed through: Docs shows [[links]], #tags,
//     ==highlights== and callouts as plain text, without backslashes.
func ToDoc(local string) string {
	local = NormalizeNewlines(local)
	var b strings.Builder
	fm, body, ok := SplitLocal(local)
	if ok {
		b.WriteString(RenderDocFM(ParseLocalFM(fm)))
		body = strings.TrimLeft(body, "\n")
	}
	b.WriteString(bodyToDoc(body))
	return b.String()
}

func bodyToDoc(body string) string {
	lines := strings.Split(body, "\n")
	inFence := false
	for i, l := range lines {
		if fenceRe.MatchString(l) {
			inFence = !inFence
			continue
		}
		if inFence || i+1 >= len(lines) {
			continue
		}
		if needsBreak(l, lines[i+1]) {
			lines[i] = l + "  "
		}
	}
	s := strings.TrimRight(strings.Join(lines, "\n"), "\n")
	if s == "" {
		return ""
	}
	return s + "\n"
}

// needsBreak reports whether cur and next are two lines of the same
// paragraph (or quote, or list item) that Docs would otherwise join.
func needsBreak(cur, next string) bool {
	if strings.TrimSpace(cur) == "" || strings.TrimSpace(next) == "" {
		return false
	}
	if strings.HasSuffix(cur, "  ") || strings.HasSuffix(cur, `\`) {
		return false
	}
	if isBlockLine(cur) || isBlockLine(next) || IsListItem(next) {
		return false
	}
	if quoteRe.MatchString(next) && !quoteRe.MatchString(cur) {
		return false
	}
	return true
}

// isBlockLine reports lines that stand on their own and never join with a
// neighbour: headings, fences, horizontal lines and table rows.
func isBlockLine(l string) bool {
	t := strings.TrimSpace(l)
	return headingRe.MatchString(t) || fenceRe.MatchString(l) || hrRe.MatchString(l) || tableRowRe.MatchString(l)
}

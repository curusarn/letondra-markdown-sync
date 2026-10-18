package mdconv

import (
	"regexp"
	"strings"

	"github.com/curusarn/letondra-markdown-sync/internal/textmerge"
)

// ApplyRemote carries the changes made in the Google Doc over to the local
// file, without ever writing the export over it.
//
// baseLocal is the local text at the last sync, baseExport and newExport are
// the cleaned exports of the Doc at the last sync and now. The result is
// baseLocal with only the parts that changed between the two exports
// replaced. Everything the Doc did not touch keeps its exact local form:
// front matter formatting, code fences, [[links]], soft line breaks.
//
// The caller then runs textmerge.Merge3(baseLocal, currentLocal, result) to
// combine it with edits made locally in the meantime.
func ApplyRemote(baseLocal, baseExport, newExport string) string {
	if baseExport == newExport {
		return baseLocal
	}
	blFM, blBody, blOK := SplitLocal(baseLocal)
	beFM, beBody, beOK := SplitExport(baseExport)
	neFM, neBody, neOK := SplitExport(newExport)

	fmLines, fmOK := blFM, blOK
	if beOK != neOK || !EqualFM(ParseExportFM(beFM), ParseExportFM(neFM)) {
		fmOK = neOK
		fmLines = nil
		if neOK {
			fmLines = RenderLocalFM(ParseExportFM(neFM), ParseLocalFM(blFM))
		}
	}

	body := applyBody(blBody, beBody, neBody)
	if !fmOK {
		if blOK {
			body = strings.TrimLeft(body, "\n")
		}
		return body
	}
	if !blOK && body != "" {
		body = "\n" + body
	}
	return "---\n" + strings.Join(fmLines, "\n") + "\n---\n" + body
}

func applyBody(bl, be, ne string) string {
	if be == ne {
		return bl
	}
	blL := strings.Split(bl, "\n")
	beL := strings.Split(be, "\n")
	neL := strings.Split(ne, "\n")

	beC, beN := contentLines(beL, false)
	neC, neN := contentLines(neL, false)
	blC, blN := contentLines(blL, true)

	// Which base-export line is which local line.
	toLocal := make([]int, len(beC))
	for i := range toLocal {
		toLocal[i] = -1
	}
	for _, op := range textmerge.Diff(beN, blN) {
		if op.Kind == textmerge.Equal {
			for k := 0; k < op.A2-op.A1; k++ {
				toLocal[op.A1+k] = blC[op.B1+k]
			}
		}
	}

	type edit struct {
		start, end int
		lines      []string
	}
	var edits []edit
	for _, op := range textmerge.Diff(beN, neN) {
		if op.Kind == textmerge.Equal {
			continue
		}
		var chunk []string
		if op.B2 > op.B1 {
			chunk = append(chunk, neL[neC[op.B1]:neC[op.B2-1]+1]...)
		}
		lo, hi := -1, -1
		for k := op.A1; k < op.A2; k++ {
			if li := toLocal[k]; li >= 0 {
				if lo < 0 {
					lo = li
				}
				hi = li + 1
			}
		}
		if lo >= 0 {
			if len(chunk) == 0 && lo > 0 && hi < len(blL) && blL[lo-1] == "" && blL[hi] == "" {
				hi++ // a deleted paragraph takes one of its blank lines along
			}
			edits = append(edits, edit{lo, hi, chunk})
			continue
		}
		if len(chunk) == 0 {
			continue // removed lines the local file does not have
		}
		// Insertion: after the local line of the nearest preceding export line.
		pos := 0
		for k := op.A1 - 1; k >= 0; k-- {
			if toLocal[k] >= 0 {
				pos = toLocal[k] + 1
				break
			}
		}
		if op.B1 > 0 && neC[op.B1]-neC[op.B1-1] > 1 {
			chunk = append([]string{""}, chunk...)
		}
		if op.B2 < len(neC) && neC[op.B2]-neC[op.B2-1] > 1 && pos < len(blL) && blL[pos] != "" {
			chunk = append(chunk, "")
		}
		edits = append(edits, edit{pos, pos, chunk})
	}
	for i := len(edits) - 1; i >= 0; i-- {
		e := edits[i]
		tail := append([]string(nil), blL[e.end:]...)
		blL = append(append(blL[:e.start], e.lines...), tail...)
	}
	return strings.Join(blL, "\n")
}

// contentLines returns the indexes of the lines that carry content and their
// normalized form for matching. Local code fences are skipped, because the
// export has no fences.
func contentLines(lines []string, local bool) ([]int, []string) {
	var idx []int
	var norm []string
	for i, l := range lines {
		if local && fenceRe.MatchString(l) {
			continue
		}
		n := Norm(l)
		if n == "" {
			continue
		}
		idx = append(idx, i)
		norm = append(norm, n)
	}
	return idx, norm
}

var (
	spacesRe    = regexp.MustCompile(`\s+`)
	bulletRe    = regexp.MustCompile(`^[*+]\s`)
	anyEscapeRe = regexp.MustCompile("\\\\([!-/:-@\\[-`{-~])")
)

// Norm reduces a line to what it means, so a local line and the same line in
// a Docs export compare equal: no escapes, no hard breaks, single spaces,
// "-" for every bullet, one table separator form, bare links.
func Norm(l string) string {
	t := strings.TrimSpace(l)
	if t == "" {
		return ""
	}
	if tableSepRe.MatchString(t) && strings.Contains(t, "|") {
		return "|---|"
	}
	t = strings.TrimSuffix(t, `\`)
	t = anyEscapeRe.ReplaceAllString(t, "$1")
	t = bareLinks(t)
	t = bulletRe.ReplaceAllString(t, "- ")
	return spacesRe.ReplaceAllString(strings.TrimSpace(t), " ")
}

// ExportToLocal turns a cleaned export into a new local file, for documents
// that appear on Drive and have no local version yet.
func ExportToLocal(clean string) string {
	fm, body, ok := SplitExport(clean)
	if !ok {
		return clean
	}
	out := "---\n" + strings.Join(RenderLocalFM(ParseExportFM(fm), nil), "\n") + "\n---\n"
	if body != "" {
		out += "\n" + body
	}
	return out
}

// SameContent reports whether a local file and a cleaned export say the same
// thing, ignoring formatting the conversion changes. Used to pair a local
// file with a Drive document the first time, when there is no base yet.
func SameContent(local, clean string) bool {
	lfm, lbody, lok := SplitLocal(NormalizeNewlines(local))
	efm, ebody, eok := SplitExport(clean)
	if lok != eok || (lok && !EqualFM(ParseLocalFM(lfm), ParseExportFM(efm))) {
		return false
	}
	_, a := contentLines(strings.Split(lbody, "\n"), true)
	_, b := contentLines(strings.Split(ebody, "\n"), false)
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

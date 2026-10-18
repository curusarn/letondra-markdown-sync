package textmerge

import "sort"

// Chunk is one piece of a 3-way merge result: either lines both sides agree
// on (Conflict == false, Lines set) or a conflict with each side's version.
type Chunk struct {
	Conflict bool
	Lines    []string // the merged lines when Conflict is false
	Base     []string // when Conflict: the common ancestor's lines
	Local    []string // when Conflict: the local (Obsidian) lines
	Remote   []string // when Conflict: the Google Drive lines
}

// Merge3 merges local and remote, both derived from base, like git does.
// Changes to different lines are combined. Overlapping changes that are not
// identical become conflict chunks. Changes that merely touch (one side edits
// line 3, the other line 4) are combined, not reported as conflicts.
func Merge3(base, local, remote []string) []Chunk {
	type hunk struct {
		o1, o2, s1, s2 int
		side           int // 0 = local, 1 = remote
	}
	var hunks []hunk
	for side, other := range [][]string{local, remote} {
		for _, op := range Diff(base, other) {
			if op.Kind != Equal {
				hunks = append(hunks, hunk{op.A1, op.A2, op.B1, op.B2, side})
			}
		}
	}
	// Order by start; at the same start, insertions before ranges.
	sort.SliceStable(hunks, func(i, j int) bool {
		a, b := hunks[i], hunks[j]
		if a.o1 != b.o1 {
			return a.o1 < b.o1
		}
		ar, br := a.o2 > a.o1, b.o2 > b.o1
		if ar != br {
			return !ar
		}
		return a.side < b.side
	})

	// Two ranges overlap only if they share a base line, so edits on
	// neighbouring lines merge cleanly. An insertion overlaps a range when it
	// sits inside it or on its edge, because then the order is ambiguous.
	overlaps := func(a1, a2, b1, b2 int) bool {
		switch {
		case a1 == a2 && b1 == b2:
			return a1 == b1
		case a1 == a2:
			return b1 <= a1 && a1 <= b2
		case b1 == b2:
			return a1 <= b1 && b1 <= a2
		}
		return a1 < b2 && b1 < a2
	}

	var out []Chunk
	emit := func(lines []string) {
		if len(lines) == 0 {
			return
		}
		if n := len(out); n > 0 && !out[n-1].Conflict {
			out[n-1].Lines = append(out[n-1].Lines, lines...)
			return
		}
		out = append(out, Chunk{Lines: append([]string(nil), lines...)})
	}

	pos := 0
	for i := 0; i < len(hunks); {
		// Grow a region of hunks that overlap each other.
		lo, hi := hunks[i].o1, hunks[i].o2
		j := i + 1
		for j < len(hunks) && hunks[j].o1 <= hi && overlaps(lo, hi, hunks[j].o1, hunks[j].o2) {
			if hunks[j].o2 > hi {
				hi = hunks[j].o2
			}
			j++
		}
		emit(base[pos:lo])
		region := hunks[i:j]
		sides := map[int]bool{}
		for _, h := range region {
			sides[h.side] = true
		}
		// Text of the region as seen by one side.
		view := func(side int, src []string) []string {
			var res []string
			p := lo
			for _, h := range region {
				if h.side != side {
					continue
				}
				res = append(res, base[p:h.o1]...)
				res = append(res, src[h.s1:h.s2]...)
				p = h.o2
			}
			return append(res, base[p:hi]...)
		}
		switch {
		case len(sides) == 1 && sides[0]:
			emit(view(0, local))
		case len(sides) == 1 && sides[1]:
			emit(view(1, remote))
		default:
			l, r := view(0, local), view(1, remote)
			if equalLines(l, r) {
				emit(l)
			} else {
				out = append(out, Chunk{
					Conflict: true,
					Base:     append([]string(nil), base[lo:hi]...),
					Local:    l,
					Remote:   r,
				})
			}
		}
		pos = hi
		i = j
	}
	emit(base[pos:])
	return out
}

// HasConflict reports whether any chunk is a conflict.
func HasConflict(chunks []Chunk) bool {
	for _, c := range chunks {
		if c.Conflict {
			return true
		}
	}
	return false
}

// Lines joins the chunks of a conflict-free merge.
func Lines(chunks []Chunk) []string {
	var res []string
	for _, c := range chunks {
		res = append(res, c.Lines...)
	}
	return res
}

func equalLines(a, b []string) bool {
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

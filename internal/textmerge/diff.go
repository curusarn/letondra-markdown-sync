// Package textmerge holds the line-based diff, the git-like 3-way merge and
// the step that carries changes made on Google Drive over to the local file.
package textmerge

// OpKind is the kind of one diff opcode.
type OpKind int

const (
	Equal OpKind = iota
	Replace
	Delete
	Insert
)

// Op says that a[A1:A2] turns into b[B1:B2]. For Equal both ranges have the
// same length and the same content.
type Op struct {
	Kind           OpKind
	A1, A2, B1, B2 int
}

// Diff returns the opcodes that turn a into b (Myers' O(ND) algorithm).
// Adjacent deletes and inserts are merged into a single Replace.
func Diff(a, b []string) []Op {
	pairs := lcsPairs(a, b)
	var ops []Op
	ai, bi := 0, 0
	flush := func(a2, b2 int) {
		switch {
		case a2 > ai && b2 > bi:
			ops = append(ops, Op{Replace, ai, a2, bi, b2})
		case a2 > ai:
			ops = append(ops, Op{Delete, ai, a2, bi, bi})
		case b2 > bi:
			ops = append(ops, Op{Insert, ai, ai, bi, b2})
		}
	}
	for _, p := range pairs {
		flush(p[0], p[1])
		if n := len(ops); n > 0 && ops[n-1].Kind == Equal && ops[n-1].A2 == p[0] && ops[n-1].B2 == p[1] {
			ops[n-1].A2++
			ops[n-1].B2++
		} else {
			ops = append(ops, Op{Equal, p[0], p[0] + 1, p[1], p[1] + 1})
		}
		ai, bi = p[0]+1, p[1]+1
	}
	flush(len(a), len(b))
	return ops
}

// lcsPairs returns the index pairs (i, j) with a[i] == b[j] that form a
// longest common subsequence, in increasing order.
func lcsPairs(a, b []string) [][2]int {
	// Trim the common prefix and suffix first: most edits are small.
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	var out [][2]int
	for i := 0; i < pre; i++ {
		out = append(out, [2]int{i, i})
	}
	for _, p := range myers(a[pre:len(a)-suf], b[pre:len(b)-suf]) {
		out = append(out, [2]int{p[0] + pre, p[1] + pre})
	}
	for i := 0; i < suf; i++ {
		out = append(out, [2]int{len(a) - suf + i, len(b) - suf + i})
	}
	return out
}

func myers(a, b []string) [][2]int {
	n, m := len(a), len(b)
	if n == 0 || m == 0 {
		return nil
	}
	max := n + m
	off := max
	v := make([]int, 2*max+2)
	var trace [][]int
	for d := 0; d <= max; d++ {
		snap := make([]int, len(v))
		copy(snap, v)
		trace = append(trace, snap)
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				x = v[off+k+1]
			} else {
				x = v[off+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[off+k] = x
			if x >= n && y >= m {
				return backtrack(trace, a, b, off, d)
			}
		}
	}
	return nil
}

func backtrack(trace [][]int, a, b []string, off, dEnd int) [][2]int {
	x, y := len(a), len(b)
	var rev [][2]int
	for d := dEnd; d > 0; d-- {
		v := trace[d]
		k := x - y
		var prevK int
		if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := v[off+prevK]
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			x--
			y--
			rev = append(rev, [2]int{x, y})
		}
		x, y = prevX, prevY
	}
	for x > 0 && y > 0 {
		x--
		y--
		rev = append(rev, [2]int{x, y})
	}
	out := make([][2]int, len(rev))
	for i := range rev {
		out[i] = rev[len(rev)-1-i]
	}
	return out
}

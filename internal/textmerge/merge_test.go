package textmerge

import (
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

func lcsLen(a, b []string) int {
	dp := make([][]int, len(a)+1)
	for i := range dp {
		dp[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] > dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	return dp[0][0]
}

// apply rebuilds b from a and the opcodes.
func apply(a, b []string, ops []Op) []string {
	var out []string
	for _, op := range ops {
		if op.Kind == Equal {
			out = append(out, a[op.A1:op.A2]...)
		} else {
			out = append(out, b[op.B1:op.B2]...)
		}
	}
	return out
}

func TestDiffRandom(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for n := 0; n < 2000; n++ {
		a := make([]string, r.Intn(12))
		b := make([]string, r.Intn(12))
		for i := range a {
			a[i] = string(rune('a' + r.Intn(4)))
		}
		for i := range b {
			b[i] = string(rune('a' + r.Intn(4)))
		}
		ops := Diff(a, b)
		if got := apply(a, b, ops); !reflect.DeepEqual(got, b) && !(len(got) == 0 && len(b) == 0) {
			t.Fatalf("apply(%v,%v) = %v", a, b, got)
		}
		eq := 0
		for _, op := range ops {
			if op.Kind == Equal {
				eq += op.A2 - op.A1
				if !reflect.DeepEqual(a[op.A1:op.A2], b[op.B1:op.B2]) {
					t.Fatalf("equal op differs: %v %v %+v", a, b, op)
				}
			}
		}
		if eq != lcsLen(a, b) {
			t.Fatalf("not minimal: %v %v: %d vs %d", a, b, eq, lcsLen(a, b))
		}
	}
}

func L(s string) []string { return strings.Split(s, "\n") }

func TestMerge3(t *testing.T) {
	cases := []struct {
		name, base, local, remote, want string
		conflict                        bool
	}{
		{"only local", "a\nb\nc", "a\nB\nc", "a\nb\nc", "a\nB\nc", false},
		{"only remote", "a\nb\nc", "a\nb\nc", "a\nb\nC", "a\nb\nC", false},
		{"different lines", "a\nb\nc\nd", "A\nb\nc\nd", "a\nb\nc\nD", "A\nb\nc\nD", false},
		{"neighbouring lines", "a\nb\nc", "a\nB\nc", "a\nb\nC", "a\nB\nC", false},
		{"same change", "a\nb", "a\nX", "a\nX", "a\nX", false},
		{"same line", "a\nb\nc", "a\nL\nc", "a\nR\nc", "", true},
		{"insert same place", "a\nc", "a\nL\nc", "a\nR\nc", "", true},
		{"insert both ends", "a\nb", "x\na\nb", "a\nb\ny", "x\na\nb\ny", false},
		{"delete vs edit", "a\nb\nc", "a\nc", "a\nB\nc", "", true},
		{"insert inside other's range", "a\nb\nc\nd", "a\nX\nY\nd", "a\nb\nN\nc\nd", "", true},
	}
	for _, c := range cases {
		ch := Merge3(L(c.base), L(c.local), L(c.remote))
		if HasConflict(ch) != c.conflict {
			t.Errorf("%s: conflict=%v, want %v (%+v)", c.name, HasConflict(ch), c.conflict, ch)
			continue
		}
		if !c.conflict {
			if got := strings.Join(Lines(ch), "\n"); got != c.want {
				t.Errorf("%s: got %q want %q", c.name, got, c.want)
			}
		}
	}
}

func TestMerge3ConflictSides(t *testing.T) {
	ch := Merge3(L("a\nb\nc"), L("a\nL\nc"), L("a\nR\nc"))
	if len(ch) != 3 || !ch[1].Conflict || ch[1].Local[0] != "L" || ch[1].Remote[0] != "R" || ch[1].Base[0] != "b" {
		t.Fatalf("unexpected chunks %+v", ch)
	}
}

func TestMerge3RandomNoPanic(t *testing.T) {
	r := rand.New(rand.NewSource(2))
	mut := func(x []string) []string {
		var out []string
		for _, s := range x {
			switch r.Intn(6) {
			case 0:
			case 1:
				out = append(out, s+"'")
			case 2:
				out = append(out, "n", s)
			default:
				out = append(out, s)
			}
		}
		return out
	}
	for n := 0; n < 3000; n++ {
		base := make([]string, r.Intn(10))
		for i := range base {
			base[i] = string(rune('a' + i))
		}
		l, rm := mut(base), mut(base)
		ch := Merge3(base, l, rm)
		if !HasConflict(ch) {
			// A clean merge of a side with an unchanged base is that side.
			if reflect.DeepEqual(rm, base) && !reflect.DeepEqual(Lines(ch), l) && len(l) > 0 {
				t.Fatalf("base %v local %v: got %v", base, l, Lines(ch))
			}
		}
	}
}

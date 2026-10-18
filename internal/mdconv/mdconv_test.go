package mdconv

import (
	"os"
	"strings"
	"testing"
)

func TestUnescape(t *testing.T) {
	cases := map[string]string{
		`\[\[Wiki odkaz\]\] a \#tag`:              `[[Wiki odkaz]] a #tag`,
		`\!\[\[obrazek.png\]\]`:                   `![[obrazek.png]]`,
		`Text s \==zvýrazněním==`:                 `Text s ==zvýrazněním==`,
		`> \[\!note\] Callout`:                    `> [!note] Callout`,
		`Cesta C:\\Users\\simon a 18\. 10\. 2026`: `Cesta C:\Users\simon a 18. 10. 2026`,
		`2\*3\*4 a snake\_case`:                   `2\*3\*4 a snake_case`,
		`\# not a heading`:                        `\# not a heading`,
		`\#tag at start`:                          `#tag at start`,
		`18\. 10\. at start`:                      `18\. 10. at start`,
		`\- not a bullet`:                         `\- not a bullet`,
		"`code \\[x\\]` and \\[y\\]":              "`code \\[x\\]` and [y]",
		`Matematika \$e=mc^2\$`:                   `Matematika $e=mc^2$`,
		`- \#tag v odrážce`:                       `- #tag v odrážce`,
		`a \\\* b`:                                `a \\\* b`,
	}
	for in, want := range cases {
		if got := Unescape(in); got != want {
			t.Errorf("Unescape(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCleanRealExport(t *testing.T) {
	raw, err := os.ReadFile("testdata/docs_export_sample.md")
	if err != nil {
		t.Fatal(err)
	}
	got := Clean(string(raw))
	for _, want := range []string{
		"- odrážka [[Wiki odkaz]] a [[Jiná|alias]] a [[Poznámka#Sekce]]\n",
		"- #tag a #obor/podtag a 2*3*4 a snake_case_slovo\n",
		"> [!note] Callout titulek obsah calloutu druhý řádek\n",
		"| --- | --- |\n",
		"| a | [[link]] |\n",
		"Cesta C:\\Users\\simon a 18. 10. 2026 a URL https://example.com/a_b_c.\n",
		"![[obrazek.png]] ![popis](https://example.com/a.png)\n",
		"Matematika $e=mc^2$ a %%komentář%%",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("clean export misses %q\n---\n%s", want, got)
		}
	}
	if strings.Contains(got, "  \n") || strings.Contains(got, "\n\n\n") {
		t.Errorf("trailing spaces or double blank lines left:\n%s", got)
	}
	if strings.Contains(got, "hotovo\n\n3.") {
		t.Errorf("blank line between list items")
	}
}

func TestToDocHardBreaks(t *testing.T) {
	in := "# H\n\nfirst line\nsecond line\n\n- item\n- item2\n\n> a\n> b\n\n```\ncode\nmore\n```\n\n| a | b |\n|---|---|\n| 1 | 2 |\n"
	want := "# H\n\nfirst line  \nsecond line\n\n- item\n- item2\n\n> a  \n> b\n\n```\ncode\nmore\n```\n\n| a | b |\n|---|---|\n| 1 | 2 |\n"
	if got := ToDoc(in); got != want {
		t.Errorf("ToDoc:\n%q\nwant\n%q", got, want)
	}
}

func TestFrontMatterToDoc(t *testing.T) {
	in := "---\ntags:\n  - lide/grulichtomas\n  - skola/psp\ndate: 2026-10-18\naliases: [Tomáš]\n---\n\n# Poznámka\n"
	got := ToDoc(in)
	want := "***\n\ntags:\n\n- lide/grulichtomas\n- skola/psp\n\ndate: 2026-10-18\n\naliases: [Tomáš]\n\n***\n\n# Poznámka\n"
	if got != want {
		t.Errorf("ToDoc fm:\n%q\nwant\n%q", got, want)
	}
}

// exportOf imitates what Docs exports for our upload of fm block (verified
// shape: see CLAUDE.md).
const fmExport = "---\n\ntags:\n\n- lide/grulichtomas  \n- skola/psp\n\ndate: 2026-10-18\n\naliases: \\[Tomáš\\]\n\n---\n\n# Poznámka\n"

func TestApplyRemoteFrontMatterUntouched(t *testing.T) {
	local := "---\ntags:\n  - lide/grulichtomas\n  - skola/psp\ndate: 2026-10-18\naliases: [Tomáš]\n---\n\n# Poznámka\n\nText.\n"
	base := Clean(fmExport + "\nText.\n")
	newer := Clean(fmExport + "\nText upravený na telefonu.\n")
	got := ApplyRemote(local, base, newer)
	want := strings.Replace(local, "Text.", "Text upravený na telefonu.", 1)
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestApplyRemoteTagsAsPlainText(t *testing.T) {
	local := "---\ntags:\n  - lide/grulichtomas\n  - skola/psp\ndate: 2026-10-18\n---\n\nText.\n"
	base := Clean("---\n\ntags:\n\n- lide/grulichtomas\n- skola/psp\n\ndate: 2026-10-18\n\n---\n\nText.\n")
	newer := Clean("---\n\ntags: lide/grulichtomas, skola/psp, novy\n\ndate: 2026-10-18\n\n---\n\nText.\n")
	got := ApplyRemote(local, base, newer)
	want := "---\ntags:\n  - lide/grulichtomas\n  - skola/psp\n  - novy\ndate: 2026-10-18\n---\n\nText.\n"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	// Same tags written as text instead of bullets: nothing changes locally.
	same := Clean("---\n\ntags: lide/grulichtomas, skola/psp\n\ndate: 2026-10-18\n\n---\n\nText.\n")
	if got := ApplyRemote(local, base, same); got != local {
		t.Errorf("reformatted tags changed local file:\n%s", got)
	}
}

func TestApplyRemoteKeepsObsidianSyntax(t *testing.T) {
	local := "# Nadpis\n\nPrvní řádek\ndruhý řádek s [[Odkaz|alias]] a #tag\n\n```python\nx = 2 * 3\n```\n\n> [!note] Callout\n> obsah\n\nPoslední odstavec.\n"
	base := Clean("# Nadpis\n\nPrvní řádek  \ndruhý řádek s \\[\\[Odkaz|alias\\]\\] a \\#tag\n\nx = 2 \\* 3\n\n> \\[\\!note\\] Callout  \n> obsah\n\nPoslední odstavec.\n")
	newer := Clean("# Nadpis\n\nPrvní řádek  \ndruhý řádek s \\[\\[Odkaz|alias\\]\\] a \\#tag\n\nx = 2 \\* 3\n\n> \\[\\!note\\] Callout  \n> obsah\n\nPoslední odstavec, upravený.\n\nNový odstavec z telefonu s \\[\\[Link\\]\\].\n")
	got := ApplyRemote(local, base, newer)
	want := "# Nadpis\n\nPrvní řádek\ndruhý řádek s [[Odkaz|alias]] a #tag\n\n```python\nx = 2 * 3\n```\n\n> [!note] Callout\n> obsah\n\nPoslední odstavec, upravený.\n\nNový odstavec z telefonu s [[Link]].\n"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestApplyRemoteInsertListItemAndDelete(t *testing.T) {
	local := "Intro\n\n- a\n- b\n\nSmazat tohle.\n\nKonec\n"
	base := Clean("Intro\n\n- a  \n- b\n\nSmazat tohle.\n\nKonec\n")
	newer := Clean("Intro\n\n- a  \n    \n- nová  \n- b\n\nKonec\n")
	got := ApplyRemote(local, base, newer)
	want := "Intro\n\n- a\n- nová\n- b\n\nKonec\n"
	if got != want {
		t.Errorf("got\n%q\nwant\n%q", got, want)
	}
}

func TestApplyRemoteAddsAndRemovesFrontMatter(t *testing.T) {
	local := "Text.\n"
	base := Clean("Text.\n")
	newer := Clean("---\n\ntags: a, b\n\n---\n\nText.\n")
	got := ApplyRemote(local, base, newer)
	want := "---\ntags:\n  - a\n  - b\n---\n\nText.\n"
	if got != want {
		t.Errorf("add fm: got %q want %q", got, want)
	}
	if back := ApplyRemote(got, newer, base); back != local {
		t.Errorf("remove fm: got %q want %q", back, local)
	}
}

func TestHorizontalLineInBodyIsContent(t *testing.T) {
	local := "Text.\n\n---\n\nDalší.\n"
	if fm, _, ok := SplitLocal(local); ok {
		t.Fatalf("body hr taken as front matter: %v", fm)
	}
	exp := Clean("Text.\n\n---\n\nDalší.\n")
	if _, _, ok := SplitExport(exp); ok {
		t.Fatalf("hr not at the start taken as front matter")
	}
}

func TestExportToLocalAndSameContent(t *testing.T) {
	exp := Clean("---\n\ntags: a, b\n\ndate: 2026-10-18\n\n---\n\n# H\n\nText \\[\\[x\\]\\].\n")
	got := ExportToLocal(exp)
	want := "---\ntags:\n  - a\n  - b\ndate: 2026-10-18\n---\n\n# H\n\nText [[x]].\n"
	if got != want {
		t.Errorf("ExportToLocal = %q, want %q", got, want)
	}
	if !SameContent(got, exp) {
		t.Errorf("SameContent false for its own conversion")
	}
	if SameContent(strings.Replace(got, "Text", "Jiný text", 1), exp) {
		t.Errorf("SameContent true for different text")
	}
}

func TestBoldHeadingFromDocs(t *testing.T) {
	if got := Clean("# **E2E poznámka**\n\nText **tučně**.\n"); got != "# E2E poznámka\n\nText **tučně**.\n" {
		t.Errorf("got %q", got)
	}
	if Norm("# **Nadpis**") != Norm("# Nadpis") {
		t.Errorf("Norm differs for bold heading")
	}
}

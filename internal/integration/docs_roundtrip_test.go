//go:build integration

// Round trips against a real Google Doc. Run with:
//
//	LMS_SA_KEY=/path/key.json LMS_TEST_DOC=<doc id> go test -tags integration ./internal/integration/
//
// LMS_TEST_DOC must be a throwaway document shared with the service account
// as Editor: the test overwrites its content.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2/google"

	"github.com/curusarn/letondra-markdown-sync/internal/gdrive"
	"github.com/curusarn/letondra-markdown-sync/internal/mdconv"
	"github.com/curusarn/letondra-markdown-sync/internal/textmerge"
)

const sample = `---
tags:
  - lide/grulichtomas
  - skola/psp
date: 2026-10-18
aliases: [Tomáš]
---

# Poznámka

První řádek odstavce
druhý řádek s [[Odkaz|alias]], [[Poznámka#Sekce]] a #tag/podtag.
Cena 2*3*4 a snake_case a 18. 10. 2026 a C:\Users\simon.

- odrážka s ==zvýrazněním==
- [ ] úkol
- [x] hotovo
  - vnořená

1. první
2. druhý

> [!note] Callout
> obsah calloutu

` + "```python\ndef f(x):\n    return x * 2\n```" + `

| Sloupec | Odkaz |
|---|---|
| a | [[link]] |

![[obrazek.png]]

https://example.com/a_b

---

Poslední odstavec.
`

func setup(t *testing.T) (context.Context, *gdrive.Client, *http.Client, gdrive.Item) {
	key, doc := os.Getenv("LMS_SA_KEY"), os.Getenv("LMS_TEST_DOC")
	if key == "" || doc == "" {
		t.Skip("LMS_SA_KEY and LMS_TEST_DOC not set")
	}
	ctx := context.Background()
	sa, _, err := gdrive.ServiceAccountClient(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(key)
	jc, err := google.JWTConfigFromJSON(raw, "https://www.googleapis.com/auth/documents")
	if err != nil {
		t.Fatal(err)
	}
	c := &gdrive.Client{SA: sa}
	it, err := c.Get(ctx, doc)
	if err != nil {
		t.Fatal(err)
	}
	if it.Name == "Example pro automat" {
		t.Fatal("refusing to overwrite the prompt document")
	}
	return ctx, c, jc.Client(ctx), it
}

func export(t *testing.T, ctx context.Context, c *gdrive.Client, it gdrive.Item) string {
	time.Sleep(2 * time.Second)
	s, err := c.Download(ctx, it)
	if err != nil {
		t.Fatal(err)
	}
	return mdconv.Clean(s)
}

// docText is what a person sees in the Google Doc.
func docText(t *testing.T, ctx context.Context, dc *http.Client, id string) string {
	resp, err := dc.Get("https://docs.googleapis.com/v1/documents/" + id)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var d struct {
		Body struct {
			Content []json.RawMessage `json:"content"`
		} `json:"body"`
	}
	var buf bytes.Buffer
	buf.ReadFrom(resp.Body)
	json.Unmarshal(buf.Bytes(), &d)
	var sb strings.Builder
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if tr, ok := x["textRun"].(map[string]any); ok {
				sb.WriteString(fmt.Sprint(tr["content"]))
			}
			for _, vv := range x {
				walk(vv)
			}
		case []any:
			for _, vv := range x {
				walk(vv)
			}
		}
	}
	var all any
	json.Unmarshal(buf.Bytes(), &all)
	walk(all.(map[string]any)["body"])
	return sb.String()
}

func batchUpdate(t *testing.T, dc *http.Client, id string, reqs ...map[string]any) {
	body, _ := json.Marshal(map[string]any{"requests": reqs})
	resp, err := dc.Post("https://docs.googleapis.com/v1/documents/"+id+":batchUpdate", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		var b bytes.Buffer
		b.ReadFrom(resp.Body)
		t.Fatalf("batchUpdate %d: %s", resp.StatusCode, b.String())
	}
}

func replaceText(find, repl string) map[string]any {
	return map[string]any{"replaceAllText": map[string]any{
		"containsText": map[string]any{"text": find, "matchCase": true},
		"replaceText":  repl,
	}}
}

func TestDocsRoundTrip(t *testing.T) {
	ctx, c, dc, it := setup(t)

	// 1. Push the Obsidian file.
	if _, err := c.Upload(ctx, it, mdconv.ToDoc(sample)); err != nil {
		t.Fatal(err)
	}
	e0 := export(t, ctx, c, it)
	t.Logf("cleaned export:\n%s", e0)

	// 2. What Simon sees in the Doc: no backslashes except the real ones.
	text := docText(t, ctx, dc, it.ID)
	if n := strings.Count(text, `\`); n != 2 { // C:\Users\simon
		t.Errorf("Doc shows %d backslashes, want 2 (C:\\Users\\simon):\n%s", n, text)
	}
	for _, want := range []string{"[[Odkaz|alias]]", "#tag/podtag", "==zvýrazněním==", "[!note] Callout", "lide/grulichtomas", "![[obrazek.png]]"} {
		if !strings.Contains(text, want) {
			t.Errorf("Doc text misses %q", want)
		}
	}
	if !strings.Contains(text, "První řádek odstavce\v") && !strings.Contains(text, "První řádek odstavce\n") {
		t.Errorf("soft line break was joined in the Doc:\n%s", text)
	}

	// 3. Every content line of the local file is found in the export.
	_, ln := lines(sample, true)
	_, en := lines(e0, false)
	matched := 0
	for _, op := range textmerge.Diff(ln, en) {
		if op.Kind == textmerge.Equal {
			matched += op.A2 - op.A1
		}
	}
	t.Logf("aligned %d of %d local content lines", matched, len(ln))
	if len(ln)-matched > 6 { // front matter and fences render differently
		t.Errorf("only %d of %d local lines align with the export", matched, len(ln))
	}

	// 4. A phone edit in the body and in the front matter.
	batchUpdate(t, dc, it.ID,
		replaceText("Poslední odstavec.", "Poslední odstavec, z telefonu."),
		replaceText("skola/psp", "skola/psp2"),
	)
	e1 := export(t, ctx, c, it)
	got := mdconv.ApplyRemote(sample, e0, e1)
	want := strings.Replace(strings.Replace(sample, "Poslední odstavec.", "Poslední odstavec, z telefonu.", 1), "skola/psp", "skola/psp2", 1)
	if got != want {
		t.Errorf("pull after phone edit:\n%s\nwant\n%s", got, want)
	}

	// 5. Phone edit merged with a local edit made at the same time.
	local := strings.Replace(sample, "# Poznámka", "# Poznámka (lokálně)", 1)
	m := textmerge.Merge3(strings.Split(sample, "\n"), strings.Split(local, "\n"), strings.Split(got, "\n"))
	if textmerge.HasConflict(m) {
		t.Fatalf("unexpected conflict: %+v", m)
	}
	merged := strings.Join(textmerge.Lines(m), "\n")
	if !strings.Contains(merged, "# Poznámka (lokálně)") || !strings.Contains(merged, "z telefonu") {
		t.Errorf("merge lost a side:\n%s", merged)
	}

	// 6. Push the merge; a pull with no Drive change leaves it untouched.
	if _, err := c.Upload(ctx, it, mdconv.ToDoc(merged)); err != nil {
		t.Fatal(err)
	}
	e2 := export(t, ctx, c, it)
	if again := mdconv.ApplyRemote(merged, e2, e2); again != merged {
		t.Errorf("no-op pull changed the file")
	}
	// The export of our own upload is stable (fixed point).
	if _, err := c.Upload(ctx, it, mdconv.ToDoc(merged)); err != nil {
		t.Fatal(err)
	}
	if e3 := export(t, ctx, c, it); e3 != e2 {
		t.Errorf("export not stable:\n%s\n---\n%s", e2, e3)
	}
}

func lines(s string, local bool) ([]int, []string) {
	var idx []int
	var out []string
	inFM := false
	for i, l := range strings.Split(s, "\n") {
		if local && i == 0 && l == "---" {
			inFM = true
			continue
		}
		if inFM {
			if l == "---" {
				inFM = false
			}
			continue
		}
		if local && strings.HasPrefix(strings.TrimSpace(l), "```") {
			continue
		}
		if n := mdconv.Norm(l); n != "" {
			idx = append(idx, i)
			out = append(out, n)
		}
	}
	return idx, out
}

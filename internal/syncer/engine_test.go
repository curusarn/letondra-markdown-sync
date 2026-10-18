package syncer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/curusarn/letondra-markdown-sync/internal/config"
	"github.com/curusarn/letondra-markdown-sync/internal/gdrive"
)

type harness struct {
	t      *testing.T
	ctx    context.Context
	d      *fakeDrive
	e      *Engine
	app    string
	root   string
	rootID string
	m      *mapping
}

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, ctx: context.Background(), d: newFakeDrive(), app: t.TempDir(), root: t.TempDir()}
	h.rootID = h.d.add("", "Markdown sync test", gdrive.MimeFolder, "")
	h.start()
	return h
}

// start (re)creates the engine from the app folder, like an app restart.
func (h *harness) start() {
	cfg := &config.Config{PollSeconds: 30, Mappings: []config.Mapping{{Local: h.root, DriveFolder: h.rootID}}}
	e, err := New(Options{Dir: h.app, Config: cfg, Remote: h.d, SignedIn: func() bool { return h.d.signedIn }})
	if err != nil {
		h.t.Fatal(err)
	}
	h.e, h.m = e, e.maps[0]
}

func (h *harness) scan()  { h.e.fullScanAll(h.ctx) }
func (h *harness) local() { h.e.localReconcile(h.ctx, h.m) }
func (h *harness) poll()  { h.e.poll(h.ctx) }

func (h *harness) read(rel string) string {
	h.t.Helper()
	b, err := os.ReadFile(filepath.Join(h.root, filepath.FromSlash(rel)))
	if err != nil {
		h.t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

func (h *harness) write(rel, text string) {
	h.t.Helper()
	p := filepath.Join(h.root, filepath.FromSlash(rel))
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		h.t.Fatal(err)
	}
	// Make sure the size/mtime shortcut sees the change.
	future := time.Now().Add(time.Duration(len(text)) * time.Millisecond)
	os.Chtimes(p, future, future)
}

func (h *harness) exists(rel string) bool {
	_, err := os.Stat(filepath.Join(h.root, filepath.FromSlash(rel)))
	return err == nil
}

func (h *harness) driveText(id string) string {
	h.d.mu.Lock()
	defer h.d.mu.Unlock()
	return h.d.content[id]
}

func (h *harness) noErrors() {
	h.t.Helper()
	h.e.publish()
	s := h.e.Status()
	if len(s.Errors) > 0 || len(s.MappingErr) > 0 {
		h.t.Fatalf("errors: %v %v", s.Errors, s.MappingErr)
	}
}

const note = "---\ntags:\n  - lide/grulichtomas\n---\n\n# Poznámka\n\nPrvní řádek s [[Odkaz]]\ndruhý řádek s #tag\n\n- a\n- b\n\nKonec.\n"

func TestInitialDownload(t *testing.T) {
	h := newHarness(t)
	sub := h.d.add(h.rootID, "Škola", gdrive.MimeFolder, "")
	h.d.add(h.rootID, "Úkoly", gdrive.MimeDoc, "# Úkoly\n\n- [ ] napsat [[Esej]]\n")
	h.d.add(sub, "Pastorace: 1", gdrive.MimeDoc, "***\n\ntags: a, b\n\n***\n\nText.\n")
	h.d.add(sub, "obrazek.png", "image/png", "binary")
	h.scan()
	h.noErrors()
	if got := h.read("Úkoly.md"); got != "# Úkoly\n\n- [ ] napsat [[Esej]]\n" {
		t.Errorf("Úkoly.md = %q", got)
	}
	if got := h.read("Škola/Pastorace꞉ 1.md"); got != "---\ntags:\n  - a\n  - b\n---\n\nText.\n" {
		t.Errorf("Pastorace = %q", got)
	}
	if h.exists("Škola/obrazek.png") || h.exists("Škola/obrazek.png.md") {
		t.Errorf("non-markdown file synced")
	}
}

func TestPushPullAndEcho(t *testing.T) {
	h := newHarness(t)
	id := h.d.add(h.rootID, "Poznámka", gdrive.MimeDoc, "")
	h.scan()
	h.write("Poznámka.md", note)
	h.local()
	h.noErrors()
	if got := h.driveText(id); !strings.Contains(got, "První řádek s [[Odkaz]]  \ndruhý řádek") || !strings.HasPrefix(got, "***\n") {
		t.Fatalf("pushed doc:\n%s", got)
	}
	// Echo: our own upload comes back through changes; nothing is written.
	info1, _ := os.Stat(filepath.Join(h.root, "Poznámka.md"))
	h.poll()
	info2, _ := os.Stat(filepath.Join(h.root, "Poznámka.md"))
	if !info1.ModTime().Equal(info2.ModTime()) {
		t.Errorf("echo rewrote the local file")
	}
	uploads := h.d.calls["upload"]
	h.poll()
	h.local()
	if h.d.calls["upload"] != uploads {
		t.Errorf("idle sync uploaded again")
	}
	// Phone edit comes down; Obsidian syntax stays.
	h.d.phoneEdit(id, func(s string) string { return strings.Replace(s, "Konec.", "Konec, z telefonu.", 1) })
	h.poll()
	h.noErrors()
	if got := h.read("Poznámka.md"); got != strings.Replace(note, "Konec.", "Konec, z telefonu.", 1) {
		t.Errorf("after phone edit:\n%s", got)
	}
}

func TestIdlePollIsOneRequest(t *testing.T) {
	h := newHarness(t)
	h.d.add(h.rootID, "A", gdrive.MimeDoc, "a\n")
	h.scan()
	h.poll()
	before := len(h.d.calls)
	total := 0
	for _, n := range h.d.calls {
		total += n
	}
	h.poll()
	after := 0
	for _, n := range h.d.calls {
		after += n
	}
	if after-total != 1 || len(h.d.calls) != before {
		t.Errorf("idle poll made %d requests", after-total)
	}
}

func TestMergeBothSides(t *testing.T) {
	h := newHarness(t)
	id := h.d.add(h.rootID, "N", gdrive.MimeDoc, "")
	h.scan()
	h.write("N.md", note)
	h.local()
	h.d.phoneEdit(id, func(s string) string { return strings.Replace(s, "Konec.", "Konec (telefon).", 1) })
	h.write("N.md", strings.Replace(note, "# Poznámka", "# Poznámka (PC)", 1))
	h.local()
	h.noErrors()
	want := strings.Replace(strings.Replace(note, "# Poznámka", "# Poznámka (PC)", 1), "Konec.", "Konec (telefon).", 1)
	if got := h.read("N.md"); got != want {
		t.Errorf("merged local:\n%s", got)
	}
	if got := h.driveText(id); !strings.Contains(got, "# Poznámka (PC)") || !strings.Contains(got, "Konec (telefon).") {
		t.Errorf("merged drive:\n%s", got)
	}
}

func TestConflictAndResolve(t *testing.T) {
	h := newHarness(t)
	var conflicts []string
	id := h.d.add(h.rootID, "N", gdrive.MimeDoc, "")
	h.scan()
	h.e.onConflct = func(id string) { conflicts = append(conflicts, id) }
	h.write("N.md", note)
	h.local()
	h.d.phoneEdit(id, func(s string) string { return strings.Replace(s, "Konec.", "Konec A.", 1) })
	h.write("N.md", strings.Replace(note, "Konec.", "Konec B.", 1))
	h.local()
	h.e.publish()
	if len(conflicts) != 1 || len(h.e.Status().Conflicts) != 1 {
		t.Fatalf("expected one conflict, got %v", conflicts)
	}
	// Nothing is overwritten while the conflict waits.
	if got := h.read("N.md"); !strings.Contains(got, "Konec B.") {
		t.Errorf("local changed during conflict")
	}
	h.local()
	h.poll()
	if len(conflicts) != 1 {
		t.Errorf("conflict reported again without changes")
	}
	f := h.e.st.Files[id]
	base, local, remote := h.e.blob(f.Conflict.Base), h.e.blob(f.Conflict.Local), h.e.blob(f.Conflict.Remote)
	if !strings.Contains(base, "Konec.") || !strings.Contains(local, "Konec B.") || !strings.Contains(remote, "Konec A.") {
		t.Fatalf("conflict versions wrong:\n%s\n%s\n%s", base, local, remote)
	}
	resolved := strings.Replace(note, "Konec.", "Konec A i B.", 1)
	if err := h.e.resolve(h.ctx, id, resolved); err != nil {
		t.Fatal(err)
	}
	h.noErrors()
	if h.read("N.md") != resolved || !strings.Contains(h.driveText(id), "Konec A i B.") {
		t.Errorf("resolution not on both sides")
	}
	h.poll()
	h.local()
	if h.read("N.md") != resolved || h.e.st.Files[id].Conflict != nil {
		t.Errorf("resolution did not stick")
	}
}

func TestResolveAfterSideChanged(t *testing.T) {
	h := newHarness(t)
	id := h.d.add(h.rootID, "N", gdrive.MimeDoc, "")
	h.scan()
	h.write("N.md", "a\nb\nc\n")
	h.local()
	h.d.phoneEdit(id, func(s string) string { return strings.Replace(s, "b", "R", 1) })
	h.write("N.md", "a\nL\nc\n")
	h.local()
	h.write("N.md", "a\nL2\nc\n")
	if err := h.e.resolve(h.ctx, id, "a\nX\nc\n"); err == nil {
		t.Fatal("resolve accepted although the local file changed")
	}
	if h.read("N.md") != "a\nL2\nc\n" {
		t.Errorf("local overwritten")
	}
}

func TestNewLocalNoteWaitsForSignIn(t *testing.T) {
	h := newHarness(t)
	h.scan()
	h.write("Nová/Poznámka.md", note)
	h.local()
	h.e.publish()
	if s := h.e.Status(); len(s.Pending) != 1 || s.State != "error" {
		t.Fatalf("status %+v", s)
	}
	h.d.signedIn = true
	h.local()
	h.noErrors()
	var doc *gdrive.Item
	for _, it := range h.d.items {
		if it.Name == "Poznámka" {
			doc = it
		}
	}
	if doc == nil {
		t.Fatal("doc not created")
	}
	folder := h.d.items[first(doc.Parents)]
	if folder.Name != "Nová" || first(folder.Parents) != h.rootID {
		t.Errorf("created in wrong folder %+v", folder)
	}
	// Further edits go to the same document, no duplicates.
	h.write("Nová/Poznámka.md", note+"\nDalší.\n")
	h.local()
	h.poll()
	n := 0
	for _, it := range h.d.items {
		if it.Name == "Poznámka" {
			n++
		}
	}
	if n != 1 || !strings.Contains(h.driveText(doc.ID), "Další.") {
		t.Errorf("duplicates or edit lost (%d docs)", n)
	}
}

func TestRenames(t *testing.T) {
	h := newHarness(t)
	h.d.signedIn = true
	id := h.d.add(h.rootID, "Stará", gdrive.MimeDoc, "Text.\n")
	h.scan()
	// Renamed in Obsidian.
	os.Rename(filepath.Join(h.root, "Stará.md"), filepath.Join(h.root, "Nová.md"))
	h.local()
	h.noErrors()
	if h.d.items[id].Name != "Nová" || len(h.d.items) != 2 {
		t.Errorf("local rename not mirrored: %+v", h.d.items[id])
	}
	// Renamed on Drive.
	h.d.renameRemote(id, "Třetí")
	h.poll()
	if !h.exists("Třetí.md") || h.exists("Nová.md") {
		t.Errorf("remote rename not mirrored")
	}
}

func TestDeletes(t *testing.T) {
	h := newHarness(t)
	h.d.signedIn = true
	a := h.d.add(h.rootID, "A", gdrive.MimeDoc, "a\n")
	b := h.d.add(h.rootID, "B", gdrive.MimeDoc, "b\n")
	h.scan()
	os.Remove(filepath.Join(h.root, "A.md"))
	h.local()
	if !h.d.trashed[a] {
		t.Errorf("local delete did not move the doc to the bin")
	}
	h.d.remove(b)
	h.poll()
	if h.exists("B.md") {
		t.Errorf("remote delete kept the local file")
	}
	backups, _ := filepath.Glob(filepath.Join(h.app, "backups", "*", "*", "B.md.*"))
	if len(backups) == 0 {
		t.Errorf("no backup of the deleted file")
	}
}

func TestRemoteDeleteKeepsLocalEdits(t *testing.T) {
	h := newHarness(t)
	h.d.signedIn = true
	b := h.d.add(h.rootID, "B", gdrive.MimeDoc, "b\n")
	h.scan()
	h.write("B.md", "b upravené\n")
	h.d.remove(b)
	h.poll()
	h.local()
	if h.read("B.md") != "b upravené\n" {
		t.Fatalf("local edit lost")
	}
	found := false
	for _, it := range h.d.items {
		if it.Name == "B" && h.driveText(it.ID) == "b upravené\n" {
			found = true
		}
	}
	if !found {
		t.Errorf("edited note not created again on Drive")
	}
}

func TestMassDeleteGuard(t *testing.T) {
	h := newHarness(t)
	h.d.signedIn = true
	for i := 0; i < 8; i++ {
		h.d.add(h.rootID, string(rune('A'+i)), gdrive.MimeDoc, "x\n")
	}
	h.scan()
	entries, _ := os.ReadDir(h.root)
	for _, en := range entries {
		os.Remove(filepath.Join(h.root, en.Name()))
	}
	h.local()
	if len(h.d.trashed) != 0 {
		t.Fatalf("deleted %d docs without confirmation", len(h.d.trashed))
	}
	h.e.publish()
	if len(h.e.Status().MappingErr) == 0 {
		t.Errorf("no warning shown")
	}
	h.e.st.MassDeleteOK[h.m.ID] = true
	h.local()
	if len(h.d.trashed) != 8 {
		t.Errorf("confirmed deletion trashed %d", len(h.d.trashed))
	}
}

func TestOfflineThenRecovers(t *testing.T) {
	h := newHarness(t)
	id := h.d.add(h.rootID, "N", gdrive.MimeDoc, "")
	h.scan()
	h.d.fail = errors.New("dial tcp: no route to host")
	h.write("N.md", "offline edit\n")
	h.local()
	h.e.publish()
	if s := h.e.Status(); s.State != "offline" || h.e.retryAt.IsZero() {
		t.Fatalf("status %+v", s)
	}
	h.d.fail = nil
	h.e.runRetries(h.ctx)
	h.noErrors()
	if h.driveText(id) != "offline edit\n" {
		t.Errorf("edit not pushed after recovery: %q", h.driveText(id))
	}
	h.e.publish()
	if s := h.e.Status(); s.State != "ok" {
		t.Errorf("status after recovery %+v", s)
	}
}

func TestRestartKeepsPairing(t *testing.T) {
	h := newHarness(t)
	id := h.d.add(h.rootID, "N", gdrive.MimeDoc, "x\n")
	h.scan()
	h.start() // app restart
	h.scan()
	h.noErrors()
	if len(h.e.st.Files) != 1 || h.e.st.Files[id] == nil {
		t.Fatalf("pairing lost: %v", h.e.st.Files)
	}
	// Lost state (deleted state.json): pairs again by name and content.
	os.Remove(filepath.Join(h.app, "state.json"))
	h.start()
	h.scan()
	h.noErrors()
	if len(h.e.st.Files) != 1 || h.read("N.md") != "x\n" || len(h.d.items) != 2 {
		t.Fatalf("re-pairing after lost state failed")
	}
}

func TestFirstRunNewerWins(t *testing.T) {
	h := newHarness(t)
	h.write("N.md", "lokální verze\n")
	os.Chtimes(filepath.Join(h.root, "N.md"), time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	h.d.add(h.rootID, "N", gdrive.MimeDoc, "drive verze\n")
	h.scan()
	if h.read("N.md") != "drive verze\n" {
		t.Errorf("newer Drive version did not win")
	}
	backups, _ := filepath.Glob(filepath.Join(h.app, "backups", "*", "*", "N.md.*"))
	if len(backups) == 0 {
		t.Errorf("older local version not backed up")
	}
}

func TestRawMarkdownFileOnDrive(t *testing.T) {
	h := newHarness(t)
	id := h.d.add(h.rootID, "Test.md", "text/markdown", "# Raw\n\n[[x]]\n")
	h.scan()
	if h.read("Test.md") != "# Raw\n\n[[x]]\n" {
		t.Fatalf("raw file not copied as is")
	}
	h.write("Test.md", "# Raw\n\n[[y]]\n")
	h.local()
	if h.driveText(id) != "# Raw\n\n[[y]]\n" {
		t.Errorf("raw push: %q", h.driveText(id))
	}
}

// Drive registers a Docs edit ~2 minutes late. A phone edit made just
// before an Obsidian save must be merged, not overwritten.
func TestLaggedPhoneEditIsMergedBeforePush(t *testing.T) {
	h := newHarness(t)
	id := h.d.add(h.rootID, "N", gdrive.MimeDoc, "")
	h.scan()
	h.write("N.md", "a\nb\nc\n")
	h.local()
	h.d.lag = true
	h.d.phoneEdit(id, func(s string) string { return strings.Replace(s, "c", "c z telefonu", 1) })
	h.write("N.md", "a z PC\nb\nc\n")
	h.local()
	h.noErrors()
	if got := h.read("N.md"); got != "a z PC\nb\nc z telefonu\n" {
		t.Errorf("local: %q", got)
	}
	if got := h.driveText(id); !strings.Contains(got, "a z PC") || !strings.Contains(got, "c z telefonu") {
		t.Errorf("drive: %q", got)
	}
	// When Drive finally registers the edit, nothing changes any more.
	h.d.flushLag()
	h.poll()
	if got := h.read("N.md"); got != "a z PC\nb\nc z telefonu\n" {
		t.Errorf("after flush: %q", got)
	}
}

func TestDeepCheckCatchesLaggedEdit(t *testing.T) {
	h := newHarness(t)
	id := h.d.add(h.rootID, "N", gdrive.MimeDoc, "")
	h.d.add(h.rootID, "Other", gdrive.MimeDoc, "x\n")
	h.scan()
	h.write("N.md", "a\nb\n")
	h.local()
	h.d.lag = true
	h.d.phoneEdit(id, func(s string) string { return strings.Replace(s, "b", "b z telefonu", 1) })
	h.poll()
	if strings.Contains(h.read("N.md"), "telefonu") {
		t.Fatal("poll saw a lagged edit; the test does not model the lag")
	}
	downloads := h.d.calls["download"]
	h.e.deepCheck(h.ctx)
	h.noErrors()
	if got := h.read("N.md"); got != "a\nb z telefonu\n" {
		t.Errorf("deep check missed the edit: %q", got)
	}
	if n := h.d.calls["download"] - downloads; n != 1 {
		t.Errorf("deep check downloaded %d documents, want only the changed one", n)
	}
}

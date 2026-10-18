package syncer

import (
	"context"
	"errors"
	"os"
	"path"
	"strings"

	"github.com/curusarn/letondra-markdown-sync/internal/gdrive"
	"github.com/curusarn/letondra-markdown-sync/internal/mdconv"
	"github.com/curusarn/letondra-markdown-sync/internal/state"
	"github.com/curusarn/letondra-markdown-sync/internal/textmerge"
)

// errLocalMoved means the local file changed while we were syncing it; the
// watcher already queued another pass.
var errLocalMoved = errors.New("local file changed during sync, trying again")

// --- conversion per kind -------------------------------------------------

func cleanExport(it gdrive.Item, raw string) string {
	if it.Kind() == gdrive.KindRaw {
		return raw
	}
	return mdconv.Clean(raw)
}

func toRemote(it gdrive.Item, local string) string {
	if it.Kind() == gdrive.KindRaw {
		return local
	}
	return mdconv.ToDoc(local)
}

func exportToLocal(it gdrive.Item, exp string) string {
	if it.Kind() == gdrive.KindRaw {
		return exp
	}
	return mdconv.ExportToLocal(exp)
}

func applyRemote(it gdrive.Item, baseLocal, baseExport, exp string) string {
	if it.Kind() == gdrive.KindRaw {
		return exp
	}
	return mdconv.ApplyRemote(baseLocal, baseExport, exp)
}

func sameContent(it gdrive.Item, local, exp string) bool {
	if it.Kind() == gdrive.KindRaw {
		return local == exp
	}
	return mdconv.SameContent(local, exp)
}

func lines(s string) []string { return strings.Split(s, "\n") }

func (e *Engine) put(text string) string {
	k, err := e.store.Put(text)
	if err != nil {
		e.logf("store blob: %v", err)
	}
	return k
}

func (e *Engine) blob(key string) string {
	s, err := e.store.Get(key)
	if err != nil {
		e.logf("read blob %s: %v", key, err)
	}
	return s
}

// download reads the document as cleaned markdown. For a Google Doc it first
// reads the Docs revisionId, so setBase can remember which revision the base
// is (an edit made between the two reads only causes one extra check).
func (e *Engine) download(ctx context.Context, it gdrive.Item) (string, error) {
	if it.Kind() == gdrive.KindDoc {
		rev, err := e.remote.DocRevision(ctx, it.ID)
		if err != nil {
			return "", err
		}
		e.lastRev[it.ID] = rev
	}
	raw, err := e.remote.Download(ctx, it)
	if err != nil {
		return "", err
	}
	return cleanExport(it, raw), nil
}

// setBase records that local text and Drive export exp (at version v) are
// in sync.
func (e *Engine) setBase(f *state.File, text, exp string, v int64, lf localFile) {
	f.BaseLocal, f.BaseExport, f.Version = e.put(text), e.put(exp), v
	f.DocRev = e.lastRev[f.DriveID]
	f.LocalSize, f.LocalMtime = lf.Size, lf.Mtime
	f.Conflict, f.LastError, f.PendingTrash = nil, "", false
	e.save()
}

// --- one tracked file ------------------------------------------------------

// syncFile brings one tracked file in line with its Drive document. item is
// the latest metadata if the caller has it; locals the local listing if the
// caller has it. depth stops retry loops.
func (e *Engine) syncFile(ctx context.Context, m *mapping, f *state.File, item *gdrive.Item, locals map[string]localFile, depth int) error {
	if depth > 2 {
		return errLocalMoved
	}
	if item == nil {
		it, err := e.remote.Get(ctx, f.DriveID)
		if gdrive.IsNotFound(err) {
			return e.remoteGone(ctx, m, f)
		}
		if err != nil {
			return err
		}
		item = &it
	}
	if item.Trashed {
		return e.remoteGone(ctx, m, f)
	}

	// Renamed or moved on Drive?
	parent := first(item.Parents)
	if item.Name != f.Name || parent != f.Parent {
		fo := e.st.Folders[parent]
		if fo == nil {
			if e.locate(ctx, *item) == m {
				e.needFull[m.ID] = true // a new folder: the full scan places it
				e.soon()
				return nil
			}
			return e.remoteGone(ctx, m, f) // moved out of the mapping
		}
		if fo.Mapping != m.ID {
			return e.remoteGone(ctx, m, f) // moved to another mapping; its scan picks it up
		}
		newRel := uniqueRel(path.Join(fo.RelPath, localName(*item)), func(r string) bool {
			if strings.EqualFold(r, f.RelPath) {
				return false
			}
			_, err := os.Stat(absPath(m.Root, r))
			return err == nil || e.trackedRel(m, r)
		})
		if newRel != f.RelPath {
			if _, err := os.Stat(absPath(m.Root, f.RelPath)); err == nil {
				if err := moveLocal(m.Root, f.RelPath, newRel); err != nil {
					return err
				}
			}
			e.logf("renamed on Drive: %s -> %s", f.RelPath, newRel)
			f.RelPath = newRel
		}
		f.Name, f.Parent = item.Name, parent
		e.save()
	}

	text, lf, err := readLocal(m.Root, f.RelPath)
	if errors.Is(err, os.ErrNotExist) {
		return e.localGone(ctx, m, f, *item, locals)
	}
	if err != nil {
		return err
	}
	localChanged := state.Hash(text) != f.BaseLocal

	exp, remoteChanged := "", false
	if item.Version != f.Version || f.Conflict != nil || e.force[f.DriveID] {
		delete(e.force, f.DriveID)
		if exp, err = e.download(ctx, *item); err != nil {
			return err
		}
		if state.Hash(exp) != f.BaseExport {
			remoteChanged = true
		} else if item.Version != f.Version {
			f.Version = item.Version // a version bump without content change
			e.save()
		}
	}

	if f.Conflict != nil && state.Hash(text) == f.Conflict.Local && state.Hash(exp) == f.Conflict.RemoteExport {
		return nil // still waiting for Simon
	}
	baseLocal, baseExport := e.blob(f.BaseLocal), e.blob(f.BaseExport)

	switch {
	case !localChanged && !remoteChanged:
		if f.Conflict != nil || f.LocalSize != lf.Size || f.LocalMtime != lf.Mtime {
			f.Conflict = nil
			f.LocalSize, f.LocalMtime = lf.Size, lf.Mtime
			e.save()
		}
		return nil
	case localChanged && !remoteChanged:
		return e.push(ctx, m, f, *item, text, depth)
	}

	if f.BaseExport == "" || baseExport == "" {
		// No usable base (a push whose export could not be read back):
		// never guess, ask.
		if sameContent(*item, text, exp) {
			e.setBase(f, text, exp, item.Version, lf)
			return nil
		}
		return e.conflict(f, text, text, exportToLocal(*item, exp), exp, item.Version)
	}

	theirs := applyRemote(*item, baseLocal, baseExport, exp)
	if !localChanged {
		return e.writePulled(m, f, *item, text, theirs, exp)
	}
	chunks := textmerge.Merge3(lines(baseLocal), lines(text), lines(theirs))
	if textmerge.HasConflict(chunks) {
		return e.conflict(f, baseLocal, text, theirs, exp, item.Version)
	}
	merged := strings.Join(textmerge.Lines(chunks), "\n")
	e.logf("merged %s (both sides changed)", f.RelPath)
	if merged != text {
		cur, _, err := readLocal(m.Root, f.RelPath)
		if err != nil {
			return err
		}
		if cur != text {
			return e.syncFile(ctx, m, f, nil, nil, depth+1)
		}
		e.backup(m.Root, f.RelPath, "before-merge")
		if _, err := writeLocal(m.Root, f.RelPath, merged); err != nil {
			return err
		}
	}
	// Drive now has `theirs`; rebase on it and push the merge.
	f.BaseLocal, f.BaseExport, f.Version = e.put(theirs), e.put(exp), item.Version
	e.save()
	return e.push(ctx, m, f, *item, merged, depth)
}

// push uploads the local text. Right before the upload it checks once more
// that the document did not change on Drive in the meantime.
func (e *Engine) push(ctx context.Context, m *mapping, f *state.File, item gdrive.Item, text string, depth int) error {
	cur, err := e.remote.Get(ctx, f.DriveID)
	if err != nil {
		return err
	}
	changed := cur.Version != item.Version
	if !changed && cur.Kind() == gdrive.KindDoc {
		// Drive registers a Docs edit ~2 minutes late; the Docs revision
		// does not. Without this check a phone edit made just before an
		// Obsidian save would be overwritten.
		rev, err := e.remote.DocRevision(ctx, f.DriveID)
		if err != nil {
			return err
		}
		changed = rev != f.DocRev
	}
	if changed {
		exp, err := e.download(ctx, cur)
		if err != nil {
			return err
		}
		if state.Hash(exp) != f.BaseExport {
			e.force[f.DriveID] = true
			return e.syncFile(ctx, m, f, &cur, nil, depth+1)
		}
		f.DocRev = e.lastRev[f.DriveID]
	}
	up, err := e.remote.Upload(ctx, cur, toRemote(cur, text))
	if err != nil {
		return err
	}
	exp, err := e.download(ctx, up)
	if err != nil {
		e.logf("uploaded %s but could not read it back: %v", f.RelPath, err)
		exp = ""
	}
	_, lf, _ := readLocal(m.Root, f.RelPath)
	e.setBase(f, text, exp, up.Version, lf)
	if exp == "" {
		f.BaseExport = ""
		e.save()
	}
	e.logf("pushed %s", f.RelPath)
	return nil
}

// writePulled writes the Drive version locally, if the local file is still
// what we merged against.
func (e *Engine) writePulled(m *mapping, f *state.File, item gdrive.Item, expect, text, exp string) error {
	if text != expect {
		cur, _, err := readLocal(m.Root, f.RelPath)
		if err != nil {
			return err
		}
		if cur != expect {
			return errLocalMoved
		}
		e.backup(m.Root, f.RelPath, "before-pull")
		if _, err := writeLocal(m.Root, f.RelPath, text); err != nil {
			return err
		}
		e.logf("pulled %s", f.RelPath)
	}
	_, lf, err := readLocal(m.Root, f.RelPath)
	if err != nil {
		return err
	}
	e.setBase(f, text, exp, item.Version, lf)
	return nil
}

// conflict records a merge that needs Simon and tells the UI.
func (e *Engine) conflict(f *state.File, base, local, remote, exp string, v int64) error {
	c := &state.Conflict{
		Base: e.put(base), Local: e.put(local), Remote: e.put(remote), RemoteExport: e.put(exp),
		RemoteVersion: v, Detected: e.now(),
	}
	isNew := f.Conflict == nil || f.Conflict.Local != c.Local || f.Conflict.Remote != c.Remote
	f.Conflict = c
	e.save()
	if isNew {
		e.logf("conflict in %s", f.RelPath)
		if e.onConflct != nil {
			e.onConflct(f.DriveID)
		}
	}
	return nil
}

// ConflictData returns the three versions of a conflict for the UI.
func (e *Engine) ConflictData(driveID string) (path, base, local, remote string, ok bool) {
	e.call(func() {
		f := e.st.Files[driveID]
		if f == nil || f.Conflict == nil {
			return
		}
		path, base, local, remote, ok = f.RelPath, e.blob(f.Conflict.Base), e.blob(f.Conflict.Local), e.blob(f.Conflict.Remote), true
	})
	return
}

// resolve applies Simon's resolution: it becomes the local file and is
// pushed to Drive. If either side changed since the conflict was shown, the
// conflict is recomputed and Simon is asked again.
func (e *Engine) resolve(ctx context.Context, id, resolved string) error {
	f := e.st.Files[id]
	if f == nil || f.Conflict == nil {
		return errors.New("tenhle soubor už v konfliktu není")
	}
	m := e.mappingByID(f.Mapping)
	if m == nil {
		return errors.New("složka už není v nastavení")
	}
	text, _, err := readLocal(m.Root, f.RelPath)
	if err != nil {
		return err
	}
	item, err := e.remote.Get(ctx, f.DriveID)
	if err != nil {
		return err
	}
	changed := state.Hash(text) != f.Conflict.Local
	if !changed && item.Version != f.Conflict.RemoteVersion {
		exp, err := e.download(ctx, item)
		if err != nil {
			return err
		}
		changed = state.Hash(exp) != f.Conflict.RemoteExport
	}
	if changed {
		_ = e.syncFile(ctx, m, f, &item, nil, 0)
		return errors.New("soubor se mezitím změnil; konflikt je přepočítaný, zkontroluj ho znovu")
	}
	resolved = mdconv.NormalizeNewlines(resolved)
	if resolved != text {
		e.backup(m.Root, f.RelPath, "before-resolve")
		if _, err := writeLocal(m.Root, f.RelPath, resolved); err != nil {
			return err
		}
	}
	// The resolution already contains the Drive side: rebase on it.
	c := f.Conflict
	f.BaseLocal, f.BaseExport, f.Version, f.Conflict = c.Remote, c.RemoteExport, c.RemoteVersion, nil
	e.save()
	e.logf("conflict resolved: %s", f.RelPath)
	if resolved == e.blob(c.Remote) {
		_, lf, _ := readLocal(m.Root, f.RelPath)
		f.LocalSize, f.LocalMtime = lf.Size, lf.Mtime
		e.save()
		return nil
	}
	if err := e.push(ctx, m, f, item, resolved, 0); err != nil {
		e.failed(f.RelPath, err, func() { e.dirtyMaps[m.ID] = true })
	}
	return nil
}

// --- appearing and disappearing files --------------------------------------

// addRemote starts tracking a Drive document that has no state yet.
func (e *Engine) addRemote(ctx context.Context, m *mapping, it gdrive.Item, rel string) error {
	exp, err := e.download(ctx, it)
	if err != nil {
		return err
	}
	f := &state.File{DriveID: it.ID, Mapping: m.ID, RelPath: rel, Name: it.Name, Parent: first(it.Parents), MimeType: it.MimeType}
	text, lf, err := readLocal(m.Root, rel)
	switch {
	case errors.Is(err, os.ErrNotExist):
		local := exportToLocal(it, exp)
		lf, err := writeLocal(m.Root, rel, local)
		if err != nil {
			return err
		}
		e.st.Files[it.ID] = f
		e.setBase(f, local, exp, it.Version, lf)
		e.logf("new from Drive: %s", rel)
		return nil
	case err != nil:
		return err
	}
	// Both exist and were never paired (first run): same content → pair;
	// otherwise the newer side wins and the other one is kept as a backup
	// (local backups folder / Drive version history).
	e.st.Files[it.ID] = f
	if sameContent(it, text, exp) {
		e.setBase(f, text, exp, it.Version, lf)
		return nil
	}
	localNewer := lf.Mtime > it.Modified.UnixNano()
	e.logf("first sync of %s: contents differ, %s is newer", rel, map[bool]string{true: "local", false: "Drive"}[localNewer])
	if localNewer {
		f.BaseLocal, f.BaseExport, f.Version = e.put(exportToLocal(it, exp)), e.put(exp), it.Version
		e.save()
		return e.push(ctx, m, f, it, text, 0)
	}
	local := exportToLocal(it, exp)
	e.backup(m.Root, rel, "replaced-by-drive")
	lf, err = writeLocal(m.Root, rel, local)
	if err != nil {
		return err
	}
	e.setBase(f, local, exp, it.Version, lf)
	return nil
}

// createRemote creates the Drive document for a new local note, as Simon.
func (e *Engine) createRemote(ctx context.Context, m *mapping, rel string) error {
	text, lf, err := readLocal(m.Root, rel)
	if err != nil {
		return err
	}
	parent, err := e.ensureFolder(ctx, m, path.Dir(rel))
	if err != nil {
		return err
	}
	it, err := e.remote.CreateDoc(ctx, parent, driveName(rel, gdrive.KindDoc), mdconv.ToDoc(text))
	if err != nil {
		return err
	}
	f := &state.File{DriveID: it.ID, Mapping: m.ID, RelPath: rel, Name: it.Name, Parent: parent, MimeType: it.MimeType}
	e.st.Files[it.ID] = f
	exp, err := e.download(ctx, it)
	if err != nil {
		exp = ""
	}
	e.setBase(f, text, exp, it.Version, lf)
	if exp == "" {
		f.BaseExport = ""
		e.save()
	}
	e.logf("created on Drive: %s", rel)
	return nil
}

// ensureFolder returns the Drive folder for a local folder path, creating
// missing folders as Simon.
func (e *Engine) ensureFolder(ctx context.Context, m *mapping, relDir string) (string, error) {
	if relDir == "." || relDir == "" {
		return m.ID, nil
	}
	for _, fo := range e.st.Folders {
		if fo.Mapping == m.ID && strings.EqualFold(fo.RelPath, relDir) {
			return fo.ID, nil
		}
	}
	parent, err := e.ensureFolder(ctx, m, path.Dir(relDir))
	if err != nil {
		return "", err
	}
	it, err := e.remote.CreateFolder(ctx, parent, path.Base(relDir))
	if err != nil {
		return "", err
	}
	e.st.Folders[it.ID] = &state.Folder{ID: it.ID, Mapping: m.ID, RelPath: relDir, Name: it.Name, Parent: parent}
	e.save()
	return it.ID, nil
}

// remoteGone handles a document deleted on Drive or moved out of the
// mapping. An unchanged local file goes to the backups folder; a locally
// edited one stays and is created on Drive again as a new document, so no
// edit is lost.
func (e *Engine) remoteGone(ctx context.Context, m *mapping, f *state.File) error {
	if _, err := e.remote.Get(ctx, m.ID); err != nil {
		if gdrive.IsRetryable(err) {
			return err
		}
		m.err = "Drive složka není dostupná. Nasdílej ji service accountu jako Editor."
		return nil // lost access to the whole folder: delete nothing
	}
	text, _, err := readLocal(m.Root, f.RelPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return err
	case state.Hash(text) == f.BaseLocal:
		if err := e.removeLocal(m.Root, f.RelPath); err != nil {
			return err
		}
		e.logf("deleted on Drive, moved local copy to backups: %s", f.RelPath)
	default:
		e.logf("deleted on Drive but edited locally, keeping it and creating it again: %s", f.RelPath)
	}
	delete(e.st.Files, f.DriveID)
	e.save()
	return nil
}

// localGone handles a tracked local file that disappeared: a rename/move in
// Obsidian (an untracked file with the same content exists) or a deletion.
func (e *Engine) localGone(ctx context.Context, m *mapping, f *state.File, item gdrive.Item, locals map[string]localFile) error {
	if locals == nil {
		var err error
		if locals, err = walkLocal(m.Root, m.Exclude); err != nil {
			return err
		}
	}
	for rel := range locals {
		if e.trackedRel(m, rel) {
			continue
		}
		text, lf, err := readLocal(m.Root, rel)
		if err != nil || state.Hash(text) != f.BaseLocal {
			continue
		}
		parent, err := e.ensureFolder(ctx, m, path.Dir(rel))
		if err != nil {
			return err
		}
		up, err := e.remote.Rename(ctx, f.DriveID, driveName(rel, item.Kind()), f.Parent, parent)
		if err != nil {
			return err
		}
		e.logf("renamed locally: %s -> %s", f.RelPath, rel)
		f.RelPath, f.Name, f.Parent = rel, up.Name, parent
		if up.Version != 0 {
			f.Version = up.Version
		}
		f.LocalSize, f.LocalMtime = lf.Size, lf.Mtime
		e.save()
		return nil
	}

	// A deletion. If Drive has edits we have not seen, they win: restore.
	if item.Version != f.Version {
		exp, err := e.download(ctx, item)
		if err != nil {
			return err
		}
		if state.Hash(exp) != f.BaseExport {
			local := applyRemote(item, e.blob(f.BaseLocal), e.blob(f.BaseExport), exp)
			lf, err := writeLocal(m.Root, f.RelPath, local)
			if err != nil {
				return err
			}
			e.logf("deleted locally but edited on Drive, restored: %s", f.RelPath)
			e.setBase(f, local, exp, item.Version, lf)
			return nil
		}
	}
	if err := e.remote.Trash(ctx, f.DriveID); err != nil {
		if errors.Is(err, gdrive.ErrNotSignedIn) {
			if !f.PendingTrash {
				f.PendingTrash = true
				e.save()
			}
			e.pending[m.ID+"|"+f.RelPath+" (smazání)"] = true
			return nil
		}
		if gdrive.IsNotFound(err) {
			err = nil
		} else {
			return err
		}
	}
	e.logf("deleted locally, moved to Drive bin: %s", f.RelPath)
	delete(e.st.Files, f.DriveID)
	e.save()
	return nil
}

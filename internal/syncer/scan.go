package syncer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/curusarn/letondra-markdown-sync/internal/gdrive"
	"github.com/curusarn/letondra-markdown-sync/internal/state"
)

// remoteDoc is a Drive document found in a mapping, with its local path.
type remoteDoc struct {
	Item gdrive.Item
	Rel  string
}

func (e *Engine) fullScanAll(ctx context.Context) {
	if e.st.PageToken == "" {
		// Take the token before scanning, so nothing that changes during
		// the scan is missed.
		tok, err := e.remote.StartPageToken(ctx)
		if err != nil {
			e.failed("Drive", err, func() {
				for _, m := range e.maps {
					e.needFull[m.ID] = true
				}
			})
			return
		}
		e.st.PageToken = tok
		e.save()
	}
	for _, m := range e.maps {
		e.fullScan(ctx, m)
	}
	e.lastPoll = e.now()
}

func checkRoot(root string) error {
	info, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("lokální složka %s neexistuje nebo není dostupná", root)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s není složka", root)
	}
	return nil
}

// fullScan lists the whole Drive folder tree of a mapping (done at start and
// when folders change) and syncs every file in it.
func (e *Engine) fullScan(ctx context.Context, m *mapping) {
	if m == nil || m.ID == "" {
		return
	}
	if err := checkRoot(m.Root); err != nil {
		m.err = err.Error()
		return
	}
	docs, folders, err := e.listTree(ctx, m)
	if err != nil {
		var ae *gdrive.APIError
		if errors.As(err, &ae) && (ae.Status == 404 || ae.Status == 403) {
			m.err = "Drive složka není dostupná. Nasdílej ji service accountu jako Editor."
			return
		}
		e.failed("Drive "+m.Root, err, func() { e.needFull[m.ID] = true })
		return
	}
	m.err = ""
	for id, fo := range e.st.Folders {
		if fo.Mapping == m.ID {
			delete(e.st.Folders, id)
		}
	}
	for _, fo := range folders {
		e.st.Folders[fo.ID] = fo
	}
	e.save()

	locals, err := walkLocal(m.Root, m.Exclude)
	if err != nil {
		m.err = err.Error()
		return
	}
	remote := map[string]remoteDoc{}
	for _, d := range docs {
		remote[d.Item.ID] = d
	}

	// Safety: if most tracked files vanished from Drive at once, something
	// is off (access lost, wrong folder). Do not delete anything locally.
	tracked, missing := 0, 0
	for _, f := range e.st.Files {
		if f.Mapping == m.ID {
			tracked++
			if _, ok := remote[f.DriveID]; !ok {
				missing++
			}
		}
	}
	allowGone := !(missing > 5 && missing*2 > tracked) || e.st.MassDeleteOK[m.ID]
	if !allowGone {
		m.err = fmt.Sprintf("Na Drive chybí %d z %d synchronizovaných dokumentů. Nic lokálně nemažu; zkontroluj sdílení složky, nebo smazání potvrď v přehledu.", missing, tracked)
	}

	for _, f := range e.filesOf(m) {
		d, ok := remote[f.DriveID]
		var err error
		if !ok {
			if allowGone {
				err = e.remoteGone(ctx, m, f)
			}
		} else {
			it := d.Item
			err = e.syncFile(ctx, m, f, &it, locals, 0)
		}
		if err != nil {
			e.failed(f.RelPath, err, func() { e.needFull[m.ID] = true })
		} else {
			e.succeeded(f.RelPath)
		}
	}
	for _, d := range docs {
		if e.st.Files[d.Item.ID] != nil {
			continue
		}
		if err := e.addRemote(ctx, m, d.Item, d.Rel); err != nil {
			e.failed(d.Rel, err, func() { e.needFull[m.ID] = true })
		}
	}
	e.createMissing(ctx, m)
}

// listTree walks the Drive folder tree of a mapping.
func (e *Engine) listTree(ctx context.Context, m *mapping) ([]remoteDoc, []*state.Folder, error) {
	root, err := e.remote.Get(ctx, m.ID)
	if err != nil {
		return nil, nil, err
	}
	folders := []*state.Folder{{ID: m.ID, Mapping: m.ID, RelPath: "", Name: root.Name}}
	var docs []remoteDoc
	taken := map[string]bool{}
	for _, f := range e.st.Files {
		if f.Mapping == m.ID {
			taken[strings.ToLower(f.RelPath)] = true
		}
	}
	for i := 0; i < len(folders); i++ {
		fo := folders[i]
		items, err := e.remote.ListFolder(ctx, fo.ID)
		if err != nil {
			return nil, nil, err
		}
		sort.Slice(items, func(a, b int) bool { return items[a].Modified.Before(items[b].Modified) })
		for _, it := range items {
			switch it.Kind() {
			case gdrive.KindFolder:
				rel := path.Join(fo.RelPath, safeName(it.Name))
				if isIgnoredName(it.Name) || excluded(rel, m.Exclude) {
					continue
				}
				folders = append(folders, &state.Folder{ID: it.ID, Mapping: m.ID, RelPath: rel, Name: it.Name, Parent: fo.ID})
			case gdrive.KindDoc, gdrive.KindRaw:
				var rel string
				if f := e.st.Files[it.ID]; f != nil && f.Mapping == m.ID {
					rel = f.RelPath // keep; syncFile handles renames
				} else {
					rel = path.Join(fo.RelPath, localName(it))
					if excluded(rel, m.Exclude) {
						continue
					}
					rel = uniqueRel(rel, func(r string) bool { return taken[r] })
					taken[strings.ToLower(rel)] = true
				}
				docs = append(docs, remoteDoc{Item: it, Rel: rel})
			}
		}
	}
	return docs, folders, nil
}

func (e *Engine) filesOf(m *mapping) []*state.File {
	var out []*state.File
	for _, f := range e.st.Files {
		if f.Mapping == m.ID {
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RelPath < out[j].RelPath })
	return out
}

// trackedRel reports whether rel is the path of a tracked file of m.
func (e *Engine) trackedRel(m *mapping, rel string) bool {
	for _, f := range e.st.Files {
		if f.Mapping == m.ID && strings.EqualFold(f.RelPath, rel) {
			return true
		}
	}
	return false
}

// localReconcile looks at a mapping's local folder after the watcher saw a
// change: pushes edited files, handles renames, deletions and new notes.
// Unchanged files are skipped by size and modification time, so this is
// cheap even for big vaults.
func (e *Engine) localReconcile(ctx context.Context, m *mapping) {
	if m == nil || m.ID == "" {
		return
	}
	if err := checkRoot(m.Root); err != nil {
		m.err = err.Error()
		return
	}
	locals, err := walkLocal(m.Root, m.Exclude)
	if err != nil {
		m.err = err.Error()
		return
	}
	files := e.filesOf(m)
	missing := 0
	for _, f := range files {
		if _, ok := locals[f.RelPath]; !ok && !f.PendingTrash {
			missing++
		}
	}
	if missing > 5 && missing*2 > len(files) && !e.st.MassDeleteOK[m.ID] {
		m.err = fmt.Sprintf("Z %s zmizelo %d z %d poznámek. Na Drive nic nemažu, dokud to nepotvrdíš v přehledu (nebo se soubory nevrátí).", m.Root, missing, len(files))
		return
	}
	if strings.HasPrefix(m.err, "Z ") {
		m.err = ""
	}
	for _, f := range files {
		if e.st.Files[f.DriveID] == nil {
			continue // removed while handling an earlier file (rename)
		}
		lf, ok := locals[f.RelPath]
		if ok && lf.Size == f.LocalSize && lf.Mtime == f.LocalMtime && f.Conflict == nil && !f.PendingTrash {
			continue
		}
		if err := e.syncFile(ctx, m, f, nil, locals, 0); err != nil {
			e.failed(f.RelPath, err, func() { e.dirtyMaps[m.ID] = true })
		} else {
			e.succeeded(f.RelPath)
		}
	}
	e.createMissing(ctx, m)
	if len(e.st.MassDeleteOK) > 0 {
		delete(e.st.MassDeleteOK, m.ID)
		e.save()
	}
}

// createMissing creates Drive documents for local notes that have none.
// That needs Simon's sign-in; without it the notes wait (status shows them).
func (e *Engine) createMissing(ctx context.Context, m *mapping) {
	locals, err := walkLocal(m.Root, m.Exclude)
	if err != nil {
		return
	}
	for k := range e.pending {
		if strings.HasPrefix(k, m.ID+"|") {
			delete(e.pending, k)
		}
	}
	rels := make([]string, 0, len(locals))
	for rel := range locals {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		if e.trackedRel(m, rel) {
			continue
		}
		if !e.signedIn() {
			e.pending[m.ID+"|"+rel] = true
			continue
		}
		if err := e.createRemote(ctx, m, rel); err != nil {
			if errors.Is(err, gdrive.ErrNotSignedIn) {
				e.pending[m.ID+"|"+rel] = true
				continue
			}
			e.failed(rel, err, func() { e.dirtyMaps[m.ID] = true })
		} else {
			e.succeeded(rel)
		}
	}
}

// poll asks Drive what changed since the last poll: one request when
// nothing did.
func (e *Engine) poll(ctx context.Context) {
	if e.st.PageToken == "" {
		e.fullScanAll(ctx)
		return
	}
	changes, next, err := e.remote.Changes(ctx, e.st.PageToken)
	if err != nil {
		var ae *gdrive.APIError
		if errors.As(err, &ae) && (ae.Status == 400 || ae.Status == 404) {
			e.logf("page token rejected (%v), starting over with a full scan", err)
			e.st.PageToken = ""
			e.fullScanAll(ctx)
			return
		}
		e.failed("Drive", err, func() { e.needPoll = true })
		return
	}
	e.lastPoll = e.now()
	if len(changes) == 0 {
		if next != e.st.PageToken {
			e.st.PageToken = next
			e.save()
		}
		return
	}
	// Folders first, so documents in new folders find their parent.
	sort.SliceStable(changes, func(i, j int) bool {
		fi := changes[i].File != nil && changes[i].File.Kind() == gdrive.KindFolder
		fj := changes[j].File != nil && changes[j].File.Kind() == gdrive.KindFolder
		return fi && !fj
	})
	full := map[string]bool{}
	ok := true
	for _, ch := range changes {
		if err := e.handleChange(ctx, ch, full); err != nil {
			ok = false
			e.failed("Drive změna", err, func() { e.needPoll = true })
		}
	}
	for id := range full {
		e.fullScan(ctx, e.mappingByID(id))
	}
	if ok {
		// Only move on when every change was handled; otherwise the same
		// changes come again next time (handling them twice is harmless).
		e.st.PageToken = next
		e.save()
	}
}

func (e *Engine) handleChange(ctx context.Context, ch gdrive.Change, full map[string]bool) error {
	if f := e.st.Files[ch.FileID]; f != nil {
		m := e.mappingByID(f.Mapping)
		if m == nil {
			return nil
		}
		if full[m.ID] {
			return nil // the full scan handles it
		}
		if ch.Removed || ch.File == nil {
			return e.remoteGone(ctx, m, f)
		}
		it := *ch.File
		return e.syncFile(ctx, m, f, &it, nil, 0)
	}
	if fo := e.st.Folders[ch.FileID]; fo != nil {
		full[fo.Mapping] = true
		return nil
	}
	if ch.Removed || ch.File == nil || ch.File.Trashed {
		return nil
	}
	it := *ch.File
	switch it.Kind() {
	case gdrive.KindFolder:
		if m := e.locate(ctx, it); m != nil {
			full[m.ID] = true
		}
	case gdrive.KindDoc, gdrive.KindRaw:
		parent := first(it.Parents)
		if fo := e.st.Folders[parent]; fo != nil {
			if full[fo.Mapping] {
				return nil
			}
			m := e.mappingByID(fo.Mapping)
			rel := path.Join(fo.RelPath, localName(it))
			if excluded(rel, m.Exclude) {
				return nil
			}
			rel = uniqueRel(rel, func(r string) bool { return e.trackedRel(m, r) })
			return e.addRemote(ctx, m, it, rel)
		}
		if m := e.locate(ctx, it); m != nil {
			full[m.ID] = true
		}
	}
	return nil
}

// locate finds the mapping an item belongs to when its parent folder is not
// known yet, by walking up the parents (a few requests, only for new items
// in new folders).
func (e *Engine) locate(ctx context.Context, it gdrive.Item) *mapping {
	id := first(it.Parents)
	for depth := 0; id != "" && depth < 10; depth++ {
		if fo := e.st.Folders[id]; fo != nil {
			return e.mappingByID(fo.Mapping)
		}
		p, err := e.remote.Get(ctx, id)
		if err != nil {
			return nil
		}
		id = first(p.Parents)
	}
	return nil
}

func first(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}

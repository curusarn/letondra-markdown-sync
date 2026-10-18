package syncer

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/curusarn/letondra-markdown-sync/internal/gdrive"
)

// fakeDrive is an in-memory Drive. Documents store the markdown that was
// uploaded; Download returns an export with the quirks of the real one
// (escaped punctuation, "---" for horizontal lines, two spaces after list
// items), so the engine's cleaning is exercised.
type fakeDrive struct {
	mu       sync.Mutex
	items    map[string]*gdrive.Item
	content  map[string]string // uploaded markdown (docs) or bytes (raw)
	changes  []string          // file IDs, index = token-1
	nextID   int
	clock    time.Time
	signedIn bool
	fail     error // returned by every call while set
	calls    map[string]int
	trashed  map[string]bool
}

func newFakeDrive() *fakeDrive {
	return &fakeDrive{items: map[string]*gdrive.Item{}, content: map[string]string{}, clock: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC), calls: map[string]int{}, trashed: map[string]bool{}}
}

func (d *fakeDrive) tick() time.Time { d.clock = d.clock.Add(time.Second); return d.clock }

func (d *fakeDrive) touch(id string) {
	it := d.items[id]
	it.Version++
	it.Modified = d.tick()
	d.changes = append(d.changes, id)
}

func (d *fakeDrive) add(parent, name, mime, content string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.nextID++
	id := fmt.Sprintf("fakeDriveID%06d", d.nextID)
	var parents []string
	if parent != "" {
		parents = []string{parent}
	}
	d.items[id] = &gdrive.Item{ID: id, Name: name, MimeType: mime, Parents: parents, Version: 1}
	d.content[id] = content
	d.touch(id)
	return id
}

// phoneEdit changes a document the way an edit in Google Docs would: the
// export changes. f gets the current export and returns the new one.
func (d *fakeDrive) phoneEdit(id string, f func(string) string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.content[id] = unexport(f(fakeExport(d.content[id])))
	d.touch(id)
}

func (d *fakeDrive) enter(name string) error {
	d.calls[name]++
	return d.fail
}

var escRe = regexp.MustCompile(`([\[\]#!=])`)

func fakeExport(md string) string {
	var out []string
	for _, l := range strings.Split(md, "\n") {
		if strings.TrimSpace(l) == "***" {
			out = append(out, "---")
			continue
		}
		prefix := ""
		if m := regexp.MustCompile(`^(#{1,6} |\s*- \[[ x]\] |\s*- )`).FindString(l); m != "" {
			prefix, l = m, l[len(m):]
		}
		l = prefix + escRe.ReplaceAllString(l, `\$1`)
		if strings.HasPrefix(strings.TrimSpace(prefix), "-") {
			l += "  "
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// unexport turns an edited export back into stored markdown.
func unexport(s string) string {
	return regexp.MustCompile(`\\([\[\]#!=])`).ReplaceAllString(s, "$1")
}

func (d *fakeDrive) StartPageToken(ctx context.Context) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.enter("start"); err != nil {
		return "", err
	}
	return strconv.Itoa(len(d.changes) + 1), nil
}

func (d *fakeDrive) Changes(ctx context.Context, token string) ([]gdrive.Change, string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.enter("changes"); err != nil {
		return nil, "", err
	}
	n, _ := strconv.Atoi(token)
	seen := map[string]bool{}
	var out []gdrive.Change
	for i := n - 1; i < len(d.changes); i++ {
		id := d.changes[i]
		if seen[id] {
			continue
		}
		seen[id] = true
		it, ok := d.items[id]
		if !ok {
			out = append(out, gdrive.Change{FileID: id, Removed: true})
			continue
		}
		c := *it
		out = append(out, gdrive.Change{FileID: id, File: &c})
	}
	return out, strconv.Itoa(len(d.changes) + 1), nil
}

func (d *fakeDrive) ListFolder(ctx context.Context, folderID string) ([]gdrive.Item, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.enter("list"); err != nil {
		return nil, err
	}
	var out []gdrive.Item
	for _, it := range d.items {
		if first(it.Parents) == folderID && !it.Trashed {
			out = append(out, *it)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (d *fakeDrive) Get(ctx context.Context, id string) (gdrive.Item, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.enter("get"); err != nil {
		return gdrive.Item{}, err
	}
	it, ok := d.items[id]
	if !ok {
		return gdrive.Item{}, &gdrive.APIError{Status: 404, Message: "not found"}
	}
	return *it, nil
}

func (d *fakeDrive) Download(ctx context.Context, it gdrive.Item) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.enter("download"); err != nil {
		return "", err
	}
	if it.Kind() == gdrive.KindRaw {
		return d.content[it.ID], nil
	}
	return fakeExport(d.content[it.ID]), nil
}

func (d *fakeDrive) Upload(ctx context.Context, it gdrive.Item, content string) (gdrive.Item, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.enter("upload"); err != nil {
		return gdrive.Item{}, err
	}
	d.content[it.ID] = content
	d.touch(it.ID)
	return *d.items[it.ID], nil
}

func (d *fakeDrive) Rename(ctx context.Context, id, name, oldParent, newParent string) (gdrive.Item, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.enter("rename"); err != nil {
		return gdrive.Item{}, err
	}
	it := d.items[id]
	it.Name = name
	if newParent != "" {
		it.Parents = []string{newParent}
	}
	d.touch(id)
	return *it, nil
}

func (d *fakeDrive) CreateDoc(ctx context.Context, parent, name, content string) (gdrive.Item, error) {
	if !d.signedIn {
		return gdrive.Item{}, gdrive.ErrNotSignedIn
	}
	if err := d.enter("create"); err != nil {
		return gdrive.Item{}, err
	}
	id := d.add(parent, name, gdrive.MimeDoc, content)
	return d.Get(ctx, id)
}

func (d *fakeDrive) CreateFolder(ctx context.Context, parent, name string) (gdrive.Item, error) {
	if !d.signedIn {
		return gdrive.Item{}, gdrive.ErrNotSignedIn
	}
	id := d.add(parent, name, gdrive.MimeFolder, "")
	return d.Get(ctx, id)
}

func (d *fakeDrive) Trash(ctx context.Context, id string) error {
	if !d.signedIn {
		return gdrive.ErrNotSignedIn
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.items[id].Trashed = true
	d.trashed[id] = true
	d.touch(id)
	return nil
}

// remove deletes an item outright (someone emptied the bin).
func (d *fakeDrive) remove(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.items, id)
	d.changes = append(d.changes, id)
}

func (d *fakeDrive) renameRemote(id, name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.items[id].Name = name
	d.touch(id)
}

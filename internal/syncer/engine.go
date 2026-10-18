// Package syncer is the sync engine. One goroutine does all the work, one
// step at a time, so two syncs of the same file can never race each other.
// Everything else (file watcher, timers, the web UI, the tray) only sends it
// requests.
package syncer

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/curusarn/letondra-markdown-sync/internal/config"
	"github.com/curusarn/letondra-markdown-sync/internal/gdrive"
	"github.com/curusarn/letondra-markdown-sync/internal/state"
)

// Remote is what the engine needs from Google Drive (gdrive.Client, or a
// fake in tests).
type Remote interface {
	StartPageToken(ctx context.Context) (string, error)
	Changes(ctx context.Context, token string) ([]gdrive.Change, string, error)
	ListFolder(ctx context.Context, folderID string) ([]gdrive.Item, error)
	Get(ctx context.Context, id string) (gdrive.Item, error)
	Download(ctx context.Context, it gdrive.Item) (string, error)
	Upload(ctx context.Context, it gdrive.Item, content string) (gdrive.Item, error)
	Rename(ctx context.Context, id, name, oldParent, newParent string) (gdrive.Item, error)
	CreateDoc(ctx context.Context, parent, name, content string) (gdrive.Item, error)
	CreateFolder(ctx context.Context, parent, name string) (gdrive.Item, error)
	Trash(ctx context.Context, id string) error
}

// mapping is a config mapping with its parsed Drive folder ID.
type mapping struct {
	ID      string // Drive folder ID of the root
	Root    string // local folder
	Exclude []string
	err     string // why this mapping cannot sync right now
}

// Status is what the tray and the web page show.
type Status struct {
	State      string // "ok", "syncing", "conflict", "error", "offline"
	Message    string
	LastSync   time.Time
	Conflicts  []ConflictInfo
	Pending    []string // local notes waiting for the Google sign-in
	Errors     []string
	SignedIn   bool
	MappingErr map[string]string
}

// ConflictInfo names one file waiting in the conflict UI.
type ConflictInfo struct {
	DriveID string
	Path    string
}

type request struct {
	kind    string // "local", "poll", "full", "resolve", "massdelete", "call"
	mapping string
	driveID string
	text    string
	fn      func() // "call": run on the engine goroutine
	reply   chan error
}

// Engine syncs the configured mappings.
type Engine struct {
	dir      string
	cfg      *config.Config
	remote   Remote
	store    *state.Store
	st       *state.State
	log      *log.Logger
	now      func() time.Time
	signedIn func() bool

	maps []*mapping

	reqs chan request

	mu       sync.Mutex
	status   Status
	listener func(Status)

	// work that failed with a retryable error and is tried again later
	dirtyMaps map[string]bool // mapping → local reconcile pending
	needPoll  bool
	needFull  map[string]bool
	backoff   time.Duration
	retryAt   time.Time
	pending   map[string]bool // "mapping|rel" of local notes waiting to be created
	fileErrs  map[string]string
	lastPoll  time.Time
	offline   string
	onConflct func(string)
}

// Options configure a new engine.
type Options struct {
	Dir        string // app folder (state, backups)
	Config     *config.Config
	Remote     Remote
	Log        *log.Logger
	Now        func() time.Time
	SignedIn   func() bool
	OnConflict func(driveID string) // a new conflict appeared
}

// New creates the engine and loads the state.
func New(o Options) (*Engine, error) {
	store, err := state.NewStore(o.Dir)
	if err != nil {
		return nil, err
	}
	st, err := store.Load()
	if err != nil {
		return nil, err
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.SignedIn == nil {
		o.SignedIn = func() bool { return false }
	}
	e := &Engine{
		dir: o.Dir, cfg: o.Config, remote: o.Remote, store: store, st: st, log: o.Log, now: o.Now,
		signedIn: o.SignedIn, onConflct: o.OnConflict,
		reqs:      make(chan request, 64),
		dirtyMaps: map[string]bool{}, needFull: map[string]bool{},
		pending: map[string]bool{}, fileErrs: map[string]string{},
	}
	for _, m := range o.Config.Mappings {
		id, err := config.FolderID(m.DriveFolder)
		mp := &mapping{ID: id, Root: m.Local, Exclude: m.Exclude}
		if err != nil {
			mp.err = err.Error()
		}
		e.maps = append(e.maps, mp)
	}
	return e, nil
}

func (e *Engine) logf(format string, args ...any) {
	if e.log != nil {
		e.log.Printf(format, args...)
	}
}

// OnStatus registers the status listener (tray, UI).
func (e *Engine) OnStatus(f func(Status)) {
	e.mu.Lock()
	e.listener = f
	e.mu.Unlock()
}

// Status returns the current status.
func (e *Engine) Status() Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.status
}

// LocalChanged tells the engine something changed in a mapping's folder.
func (e *Engine) LocalChanged(mappingID string) { e.send(request{kind: "local", mapping: mappingID}) }

// Poll asks Drive for changes now (wake-up, back at the computer, manual).
func (e *Engine) Poll() { e.send(request{kind: "poll"}) }

// SyncAll runs a full scan of every mapping.
func (e *Engine) SyncAll() { e.send(request{kind: "full"}) }

// Resolve stores Simon's resolution of a conflict and syncs it.
func (e *Engine) Resolve(driveID, text string) error {
	r := request{kind: "resolve", driveID: driveID, text: text, reply: make(chan error, 1)}
	e.send(r)
	return <-r.reply
}

// ConfirmMassDelete lets a mapping delete many files on Drive.
func (e *Engine) ConfirmMassDelete(mappingID string) {
	e.send(request{kind: "massdelete", mapping: mappingID})
}

func (e *Engine) send(r request) {
	select {
	case e.reqs <- r:
	default:
		// The queue is full of the same kinds of requests; dropping this one
		// is safe because the queued ones do the same work.
		if r.reply != nil {
			r.reply <- errors.New("engine busy, try again")
		}
	}
}

// Run is the engine loop. It returns when ctx is cancelled.
func (e *Engine) Run(ctx context.Context) {
	e.pruneBackups()
	e.store.GC(e.st)
	e.fullScanAll(ctx)
	poll := time.NewTicker(time.Duration(e.cfg.PollSeconds) * time.Second)
	defer poll.Stop()
	safety := time.NewTicker(5 * time.Minute)
	defer safety.Stop()
	debounce := time.NewTimer(time.Hour)
	debounce.Stop()
	retry := time.NewTimer(time.Hour)
	retry.Stop()
	localDue := map[string]bool{}

	for {
		e.publish()
		if !e.retryAt.IsZero() {
			retry.Reset(time.Until(e.retryAt))
		}
		select {
		case <-ctx.Done():
			return
		case r := <-e.reqs:
			switch r.kind {
			case "local":
				localDue[r.mapping] = true
				debounce.Reset(700 * time.Millisecond)
			case "poll":
				e.poll(ctx)
			case "full":
				e.fullScanAll(ctx)
			case "resolve":
				r.reply <- e.resolve(ctx, r.driveID, r.text)
			case "call":
				r.fn()
			case "massdelete":
				e.st.MassDeleteOK[r.mapping] = true
				e.save()
				e.localReconcile(ctx, e.mappingByID(r.mapping))
			}
		case <-debounce.C:
			for id := range localDue {
				e.localReconcile(ctx, e.mappingByID(id))
			}
			localDue = map[string]bool{}
		case <-poll.C:
			e.poll(ctx)
		case <-safety.C:
			// The watcher can miss events (Windows drops them when its
			// buffer overflows), so look at the folders now and then.
			for _, m := range e.maps {
				e.localReconcile(ctx, m)
			}
		case <-retry.C:
			e.retryAt = time.Time{}
			e.runRetries(ctx)
		}
	}
}

func (e *Engine) mappingByID(id string) *mapping {
	for _, m := range e.maps {
		if m.ID == id {
			return m
		}
	}
	return nil
}

// failed records an error. Retryable errors schedule another attempt with
// exponential backoff (2 s up to 5 min); others are shown to Simon.
func (e *Engine) failed(what string, err error, retry func()) {
	e.logf("%s: %v", what, err)
	if gdrive.IsRetryable(err) {
		retry()
		if e.backoff == 0 {
			e.backoff = 2 * time.Second
		} else if e.backoff < 5*time.Minute {
			e.backoff *= 2
		}
		if e.backoff > 5*time.Minute {
			e.backoff = 5 * time.Minute
		}
		e.retryAt = e.now().Add(e.backoff)
		e.setOffline(fmt.Sprintf("%s: %v (zkusím znovu za %s)", what, err, e.backoff.Round(time.Second)))
		return
	}
	e.fileErrs[what] = err.Error()
}

func (e *Engine) succeeded(what string) {
	delete(e.fileErrs, what)
}

func (e *Engine) runRetries(ctx context.Context) {
	full, dirty, poll := e.needFull, e.dirtyMaps, e.needPoll
	e.needFull, e.dirtyMaps, e.needPoll = map[string]bool{}, map[string]bool{}, false
	for id := range full {
		e.fullScan(ctx, e.mappingByID(id))
	}
	for id := range dirty {
		e.localReconcile(ctx, e.mappingByID(id))
	}
	if poll {
		e.poll(ctx)
	}
	if len(e.needFull) == 0 && len(e.dirtyMaps) == 0 && !e.needPoll {
		e.backoff = 0
		e.clearOffline()
	}
}

func (e *Engine) save() {
	if err := e.store.Save(e.st); err != nil {
		e.logf("save state: %v", err)
		e.fileErrs["state"] = "Nejde uložit stav synchronizace: " + err.Error()
	}
}

// --- status ---

func (e *Engine) setOffline(msg string) { e.offline = msg }
func (e *Engine) clearOffline()         { e.offline = "" }

// soon makes the retry timer fire shortly, for work queued in needFull,
// dirtyMaps or needPoll outside of an error.
func (e *Engine) soon() {
	if e.retryAt.IsZero() {
		e.retryAt = e.now().Add(time.Second)
	}
}

// call runs fn on the engine goroutine and waits for it. The UI uses it to
// read the state, which only that goroutine may touch.
func (e *Engine) call(fn func()) {
	done := make(chan struct{})
	e.reqs <- request{kind: "call", fn: func() { fn(); close(done) }}
	<-done
}

func (e *Engine) publish() {
	s := Status{SignedIn: e.signedIn(), MappingErr: map[string]string{}}
	for _, f := range e.st.Files {
		if f.Conflict != nil {
			s.Conflicts = append(s.Conflicts, ConflictInfo{DriveID: f.DriveID, Path: f.RelPath})
		}
	}
	sort.Slice(s.Conflicts, func(i, j int) bool { return s.Conflicts[i].Path < s.Conflicts[j].Path })
	for k := range e.pending {
		s.Pending = append(s.Pending, k[strings.Index(k, "|")+1:])
	}
	sort.Strings(s.Pending)
	for k, v := range e.fileErrs {
		s.Errors = append(s.Errors, k+": "+v)
	}
	sort.Strings(s.Errors)
	for _, m := range e.maps {
		if m.err != "" {
			s.MappingErr[m.Root] = m.err
		}
	}
	s.LastSync = e.lastPoll
	switch {
	case len(s.Conflicts) > 0:
		s.State, s.Message = "conflict", fmt.Sprintf("%d konflikt(ů) čeká na vyřešení", len(s.Conflicts))
	case len(s.MappingErr) > 0 || len(s.Errors) > 0:
		s.State, s.Message = "error", "Něco nejde synchronizovat, otevři přehled"
	case e.offline != "":
		s.State, s.Message = "offline", e.offline
	case len(s.Pending) > 0 && !s.SignedIn:
		s.State, s.Message = "error", fmt.Sprintf("%d nových poznámek čeká na přihlášení Googlem", len(s.Pending))
	default:
		s.State, s.Message = "ok", "Synchronizováno"
	}
	e.mu.Lock()
	e.status = s
	l := e.listener
	e.mu.Unlock()
	if l != nil {
		l(s)
	}
}

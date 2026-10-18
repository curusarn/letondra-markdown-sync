// Command letondra-markdown-sync keeps Google Drive folders (Google Docs) and
// local Obsidian markdown folders in sync. See README.md and CLAUDE.md.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/curusarn/letondra-markdown-sync/internal/config"
	"github.com/curusarn/letondra-markdown-sync/internal/gdrive"
	"github.com/curusarn/letondra-markdown-sync/internal/logx"
	"github.com/curusarn/letondra-markdown-sync/internal/platform"
	"github.com/curusarn/letondra-markdown-sync/internal/syncer"
	"github.com/curusarn/letondra-markdown-sync/internal/tray"
	"github.com/curusarn/letondra-markdown-sync/internal/ui"
	"github.com/curusarn/letondra-markdown-sync/internal/watch"
)

// version is set by the build (-ldflags "-X main.version=...").
var version = "dev"

// uiPort is fixed so a second start finds the running instance.
const uiPort = 47613

type app struct {
	dir string
	log *log.Logger

	mu       sync.Mutex
	cfg      *config.Config
	engine   *syncer.Engine
	stop     func()
	ua       *gdrive.UserAuth
	srv      *ui.Server
	opened   map[string]time.Time // conflict pages opened recently
	noLaunch bool
}

func main() {
	dirFlag := flag.String("dir", "", "app folder (default %APPDATA%\\LetondraMarkdownSync)")
	noLaunch := flag.Bool("no-launch", false, "do not start Obsidian")
	console := flag.Bool("console", false, "also log to the console")
	flag.Parse()
	if *dirFlag != "" {
		os.Setenv("LMS_HOME", *dirFlag)
	}
	if flag.Arg(0) == "version" {
		fmt.Println(version)
		return
	}
	dir, err := config.Dir()
	if err != nil {
		fatal(err)
	}
	var extra *os.File
	if *console {
		extra = os.Stderr
	}
	var lg *log.Logger
	if extra != nil {
		lg = logx.New(dir, extra)
	} else {
		lg = logx.New(dir, nil)
	}
	a := &app{dir: dir, log: lg, opened: map[string]time.Time{}, noLaunch: *noLaunch}

	if flag.Arg(0) == "signin" {
		// For testing the Google sign-in without the tray.
		a.loadConfig()
		if a.ua == nil {
			fatal(errors.New("config has no oauth_client"))
		}
		err := a.ua.SignIn(context.Background(), func(u string) error {
			fmt.Println("Open this URL to sign in:\n" + u)
			return platform.OpenURL(u)
		})
		if err != nil {
			fatal(err)
		}
		fmt.Println("signed in")
		return
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", uiPort))
	if err != nil {
		// Already running: open its overview and quit.
		if tok, err2 := os.ReadFile(filepath.Join(dir, "ui-token")); err2 == nil {
			platform.OpenURL(fmt.Sprintf("http://127.0.0.1:%d/?t=%s", uiPort, strings.TrimSpace(string(tok))))
			return
		}
		fatal(fmt.Errorf("port %d is taken by another program: %w", uiPort, err))
	}
	token := randomToken()
	os.WriteFile(filepath.Join(dir, "ui-token"), []byte(token), 0o600)
	a.srv = ui.New(a, ln, token)
	go func() { lg.Printf("ui: %v", a.srv.Serve()) }()

	lg.Printf("start %s, app folder %s", version, dir)
	created := a.loadConfig()
	if !a.noLaunch {
		if err := platform.Launch(a.cfg.Launch); err != nil {
			lg.Printf("launch %q: %v", a.cfg.Launch, err)
		}
	}
	if created || len(a.cfg.Validate()) > 0 {
		platform.OpenURL(a.srv.URL("/settings"))
	}
	a.restartEngine()
	go a.watchActivity()

	tray.Run(tray.Actions{
		Open:    func() { platform.OpenURL(a.srv.URL("/")) },
		SyncNow: a.SyncNow,
		SignIn:  func() { go a.SignIn() },
		OpenLog: a.OpenLog,
		Quit: func() {
			a.mu.Lock()
			if a.stop != nil {
				a.stop()
			}
			a.mu.Unlock()
			lg.Printf("quit")
		},
	}, func() {})
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	if d, e := config.Dir(); e == nil {
		os.WriteFile(filepath.Join(d, "fatal.txt"), []byte(time.Now().Format(time.RFC3339)+" "+err.Error()+"\n"), 0o600)
	}
	os.Exit(1)
}

func randomToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// loadConfig reads config.json and the Google sign-in.
func (a *app) loadConfig() (created bool) {
	cfg, created, err := config.Load(a.dir)
	if err != nil {
		a.log.Printf("config: %v", err)
		cfg = &config.Config{Launch: config.DefaultLaunch, PollSeconds: 30}
	}
	a.mu.Lock()
	a.cfg = cfg
	a.ua = nil
	if cfg.OAuthClient != "" {
		store := gdrive.TokenStore{Path: filepath.Join(a.dir, "google-token.bin"), Protect: platform.Protect, Unprotect: platform.Unprotect}
		ua, err := gdrive.NewUserAuth(context.Background(), cfg.OAuthClient, store)
		if err != nil {
			a.log.Printf("oauth client: %v", err)
		}
		a.ua = ua
	}
	a.mu.Unlock()
	return created
}

// restartEngine (re)starts syncing with the current config.
func (a *app) restartEngine() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stop != nil {
		a.stop()
		a.stop, a.engine = nil, nil
	}
	if probs := a.cfg.Validate(); len(probs) > 0 {
		a.log.Printf("not syncing: %s", strings.Join(probs, " "))
		tray.Set("error", "Dokonči nastavení")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	sa, email, err := gdrive.ServiceAccountClient(ctx, a.cfg.ServiceAccountKey)
	if err != nil {
		cancel()
		a.log.Printf("service account: %v", err)
		tray.Set("error", "Klíč service accountu nejde načíst")
		return
	}
	a.log.Printf("service account %s", email)
	ua := a.ua
	client := &gdrive.Client{SA: &http.Client{Transport: sa.Transport, Timeout: 60 * time.Second}, User: func() *http.Client {
		if c := ua.Client(); c != nil {
			return &http.Client{Transport: c.Transport, Timeout: 60 * time.Second}
		}
		return nil
	}}
	e, err := syncer.New(syncer.Options{
		Dir: a.dir, Config: a.cfg, Remote: client, Log: a.log,
		SignedIn:   ua.SignedIn,
		OnConflict: a.openConflict,
	})
	if err != nil {
		cancel()
		a.log.Printf("engine: %v", err)
		tray.Set("error", err.Error())
		return
	}
	e.OnStatus(func(s syncer.Status) { tray.Set(s.State, s.Message) })
	var stops []func()
	for _, m := range a.cfg.Mappings {
		id, _ := config.FolderID(m.DriveFolder)
		stop, err := watch.Tree(m.Local, func() { e.LocalChanged(id) }, func(err error) { a.log.Printf("watch %s: %v", m.Local, err) })
		if err != nil {
			a.log.Printf("watch %s: %v", m.Local, err)
			continue
		}
		stops = append(stops, stop)
	}
	go e.Run(ctx)
	a.engine = e
	a.stop = func() {
		for _, s := range stops {
			s()
		}
		cancel()
	}
}

// watchActivity pulls from Drive as soon as Simon is back at the computer
// (input after 2+ minutes idle) or the computer wakes up from sleep (the
// wall clock jumped), so phone edits arrive before he starts typing.
func (a *app) watchActivity() {
	prevIdle := 0
	last := time.Now()
	for range time.Tick(5 * time.Second) {
		idle := platform.IdleSeconds()
		now := time.Now()
		woke := now.Sub(last) > 60*time.Second
		back := prevIdle >= 120 && idle < 10
		if woke || back {
			a.log.Printf("back at the computer (woke=%v), checking Drive", woke)
			a.SyncNow()
		}
		prevIdle, last = idle, now
	}
}

func (a *app) openConflict(id string) {
	a.mu.Lock()
	t, seen := a.opened[id]
	if seen && time.Since(t) < 10*time.Minute {
		a.mu.Unlock()
		return
	}
	a.opened[id] = time.Now()
	a.mu.Unlock()
	platform.OpenURL(a.srv.URL("/conflict?id=" + id))
}

// --- ui.Backend ---

func (a *app) eng() *syncer.Engine {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.engine
}

func (a *app) Status() syncer.Status {
	if e := a.eng(); e != nil {
		return e.Status()
	}
	return syncer.Status{State: "error", Message: "Synchronizace neběží: dokonči nastavení"}
}

func (a *app) ConflictData(id string) (string, string, string, string, bool) {
	if e := a.eng(); e != nil {
		return e.ConflictData(id)
	}
	return "", "", "", "", false
}

func (a *app) Resolve(id, text string) error {
	if e := a.eng(); e != nil {
		return e.Resolve(id, text)
	}
	return errors.New("synchronizace neběží")
}

func (a *app) SyncNow() {
	if e := a.eng(); e != nil {
		e.Poll()
		e.SyncAll()
	}
}

func (a *app) SignIn() error {
	a.mu.Lock()
	ua := a.ua
	a.mu.Unlock()
	if ua == nil {
		err := errors.New("v nastavení chybí JSON OAuth klienta (Desktop app)")
		a.log.Printf("sign-in: %v", err)
		platform.OpenURL(a.srv.URL("/settings"))
		return err
	}
	if err := ua.SignIn(context.Background(), platform.OpenURL); err != nil {
		a.log.Printf("sign-in: %v", err)
		return err
	}
	a.log.Printf("signed in with Google")
	a.SyncNow()
	return nil
}

func (a *app) SignOut() {
	a.mu.Lock()
	ua := a.ua
	a.mu.Unlock()
	if ua != nil {
		ua.SignOut()
	}
}

func (a *app) Config() config.Config {
	a.mu.Lock()
	defer a.mu.Unlock()
	return *a.cfg
}

func (a *app) SaveConfig(c config.Config) error {
	if err := config.Save(a.dir, &c); err != nil {
		return err
	}
	a.loadConfig()
	a.restartEngine()
	return nil
}

func (a *app) ConfirmMassDelete(root string) {
	cfg := a.Config()
	for _, m := range cfg.Mappings {
		if m.Local == root {
			if id, err := config.FolderID(m.DriveFolder); err == nil {
				if e := a.eng(); e != nil {
					e.ConfirmMassDelete(id)
				}
			}
		}
	}
}

func (a *app) OpenLog() { platform.OpenFile(logx.Path(a.dir)) }
func (a *app) OpenBackups() {
	os.MkdirAll(filepath.Join(a.dir, "backups"), 0o700)
	platform.OpenFile(filepath.Join(a.dir, "backups"))
}
func (a *app) Version() string { return version }

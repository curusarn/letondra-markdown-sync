// Package ui is the app's small web interface on 127.0.0.1: an overview,
// the settings and the side-by-side conflict resolver. It is opened in the
// default browser from the tray menu, and by itself when a conflict appears.
//
// Every request must carry the session token (cookie or ?t=), so other web
// pages open in the browser cannot drive the app.
package ui

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"html/template"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/curusarn/letondra-markdown-sync/internal/config"
	"github.com/curusarn/letondra-markdown-sync/internal/syncer"
	"github.com/curusarn/letondra-markdown-sync/internal/textmerge"
)

//go:embed templates/*.html
var files embed.FS

// Backend is what the UI needs from the app.
type Backend interface {
	Status() syncer.Status
	ConflictData(id string) (path, base, local, remote string, ok bool)
	Resolve(id, text string) error
	SyncNow()
	SignIn() error
	SignOut()
	Config() config.Config
	SaveConfig(config.Config) error
	ConfirmMassDelete(root string)
	OpenLog()
	OpenBackups()
	Version() string
}

// Server serves the UI.
type Server struct {
	b     Backend
	token string
	tpl   *template.Template
	ln    net.Listener
}

// New creates the server on an existing listener.
func New(b Backend, ln net.Listener, token string) *Server {
	tpl := template.Must(template.New("").Funcs(template.FuncMap{
		"ago": func(t time.Time) string {
			if t.IsZero() {
				return "zatím ne"
			}
			return t.Format("15:04:05")
		},
		"hasPrefix": strings.HasPrefix,
		"folderURL": func(s string) string {
			id, err := config.FolderID(s)
			if err != nil {
				return ""
			}
			return "https://drive.google.com/drive/folders/" + id
		},
	}).ParseFS(files, "templates/*.html"))
	return &Server{b: b, token: token, tpl: tpl, ln: ln}
}

// URL is the address to open in a browser (with the token).
func (s *Server) URL(path string) string {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return "http://" + s.ln.Addr().String() + path + sep + "t=" + s.token
}

// Serve blocks serving requests.
func (s *Server) Serve() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.auth(s.home))
	mux.HandleFunc("/settings", s.auth(s.settings))
	mux.HandleFunc("/conflict", s.auth(s.conflict))
	mux.HandleFunc("/api/status", s.auth(s.apiStatus))
	mux.HandleFunc("/api/resolve", s.auth(s.apiResolve))
	mux.HandleFunc("/api/action", s.auth(s.apiAction))
	return http.Serve(s.ln, mux)
}

func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := r.URL.Query().Get("t")
		if c, err := r.Cookie("lms"); err == nil && tok == "" {
			tok = c.Value
		}
		if subtle.ConstantTimeCompare([]byte(tok), []byte(s.token)) != 1 {
			http.Error(w, "Otevři přehled z ikony v liště (tray).", http.StatusForbidden)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "lms", Value: s.token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
		if r.Method == http.MethodPost && r.Header.Get("X-LMS") != "1" && r.FormValue("t") != s.token {
			http.Error(w, "missing header", http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		h(w, r)
	}
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	cfg := s.b.Config()
	s.render(w, "home.html", map[string]any{
		"S": s.b.Status(), "Cfg": cfg, "Problems": cfg.Validate(), "Version": s.b.Version(), "Token": s.token,
	})
}

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	msg := ""
	if r.Method == http.MethodPost {
		r.ParseForm()
		cfg := s.b.Config()
		cfg.ServiceAccountKey = strings.TrimSpace(r.FormValue("key"))
		cfg.OAuthClient = strings.TrimSpace(r.FormValue("oauth"))
		cfg.Launch = strings.TrimSpace(r.FormValue("launch"))
		cfg.Mappings = nil
		locals, drives, excl := r.Form["local"], r.Form["drive"], r.Form["exclude"]
		for i := range locals {
			l, d := strings.TrimSpace(locals[i]), strings.TrimSpace(drives[i])
			if l == "" && d == "" {
				continue
			}
			m := config.Mapping{Local: l, DriveFolder: d}
			if i < len(excl) {
				for _, x := range strings.Split(excl[i], ",") {
					if x = strings.TrimSpace(x); x != "" {
						m.Exclude = append(m.Exclude, x)
					}
				}
			}
			m.DriveFolder = d
			if id, err := config.FolderID(d); err == nil {
				m.DriveFolder = id
			}
			cfg.Mappings = append(cfg.Mappings, m)
		}
		if err := s.b.SaveConfig(cfg); err != nil {
			msg = "Uložení selhalo: " + err.Error()
		} else {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
	}
	cfg := s.b.Config()
	rows := append([]config.Mapping{}, cfg.Mappings...)
	rows = append(rows, config.Mapping{})
	s.render(w, "settings.html", map[string]any{"Cfg": cfg, "Rows": rows, "Msg": msg, "Token": s.token})
}

// chunkView is one block of the conflict page.
type chunkView struct {
	Conflict bool
	Lines    []string // agreed lines (no conflict)
	Local    []string
	Remote   []string
}

func (s *Server) conflict(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	path, base, local, remote, ok := s.b.ConflictData(id)
	if !ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	var chunks []chunkView
	for _, c := range textmerge.Merge3(strings.Split(base, "\n"), strings.Split(local, "\n"), strings.Split(remote, "\n")) {
		chunks = append(chunks, chunkView{Conflict: c.Conflict, Lines: nonNil(c.Lines), Local: nonNil(c.Local), Remote: nonNil(c.Remote)})
	}
	data, _ := json.Marshal(chunks)
	s.render(w, "conflict.html", map[string]any{"ID": id, "Path": path, "Chunks": template.JS(data), "Token": s.token})
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func (s *Server) apiStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.b.Status())
}

func (s *Server) apiResolve(w http.ResponseWriter, r *http.Request) {
	var req struct{ ID, Text string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if err := s.b.Resolve(req.ID, req.Text); err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	w.Write([]byte("ok"))
}

func (s *Server) apiAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	switch r.FormValue("do") {
	case "sync":
		s.b.SyncNow()
	case "signin":
		go s.b.SignIn()
	case "signout":
		s.b.SignOut()
	case "confirm-delete":
		s.b.ConfirmMassDelete(r.FormValue("root"))
	case "log":
		s.b.OpenLog()
	case "backups":
		s.b.OpenBackups()
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

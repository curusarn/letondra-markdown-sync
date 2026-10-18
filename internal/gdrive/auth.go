package gdrive

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// ServiceAccountClient builds the HTTP client for the service account key.
func ServiceAccountClient(ctx context.Context, keyPath string) (*http.Client, string, error) {
	key, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, "", fmt.Errorf("read service account key: %w", err)
	}
	cfg, err := google.JWTConfigFromJSON(key, "https://www.googleapis.com/auth/drive")
	if err != nil {
		return nil, "", fmt.Errorf("parse service account key: %w", err)
	}
	return cfg.Client(ctx), cfg.Email, nil
}

// UserScope is the scope of Simon's own sign-in. Full drive access is needed
// to move existing documents to the bin; creating alone would work with
// drive.file.
const UserScope = "https://www.googleapis.com/auth/drive"

// TokenStore saves and loads the signed-in user's token. On Windows the file
// is encrypted with DPAPI (see platform.Protect).
type TokenStore struct {
	Path      string
	Protect   func([]byte) ([]byte, error)
	Unprotect func([]byte) ([]byte, error)
}

func (s TokenStore) Load() (*oauth2.Token, error) {
	data, err := os.ReadFile(s.Path)
	if err != nil {
		return nil, err
	}
	if s.Unprotect != nil {
		if data, err = s.Unprotect(data); err != nil {
			return nil, fmt.Errorf("decrypt token: %w", err)
		}
	}
	var t oauth2.Token
	return &t, json.Unmarshal(data, &t)
}

func (s TokenStore) Save(t *oauth2.Token) error {
	data, err := json.Marshal(t)
	if err != nil {
		return err
	}
	if s.Protect != nil {
		if data, err = s.Protect(data); err != nil {
			return err
		}
	}
	tmp := s.Path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.Path)
}

func (s TokenStore) Delete() error { return os.Remove(s.Path) }

// UserAuth holds Simon's sign-in: the OAuth client from the downloaded
// "Desktop app" JSON and the stored token.
type UserAuth struct {
	cfg   *oauth2.Config
	store TokenStore

	mu     sync.Mutex
	client *http.Client
	err    error // last refresh error, e.g. invalid_grant after revocation
}

// NewUserAuth reads the OAuth client JSON. It loads a stored token if there
// is one; Client() returns nil until Simon signs in.
func NewUserAuth(ctx context.Context, clientJSONPath string, store TokenStore) (*UserAuth, error) {
	data, err := os.ReadFile(clientJSONPath)
	if err != nil {
		return nil, fmt.Errorf("read OAuth client file: %w", err)
	}
	cfg, err := google.ConfigFromJSON(data, UserScope)
	if err != nil {
		return nil, fmt.Errorf("parse OAuth client file (needs a \"Desktop app\" client): %w", err)
	}
	ua := &UserAuth{cfg: cfg, store: store}
	if tok, err := store.Load(); err == nil {
		ua.setToken(ctx, tok)
	}
	return ua, nil
}

// savingSource persists refreshed tokens and remembers refresh failures.
type savingSource struct {
	ua   *UserAuth
	base oauth2.TokenSource
	last string
}

func (s *savingSource) Token() (*oauth2.Token, error) {
	t, err := s.base.Token()
	if err != nil {
		s.ua.mu.Lock()
		s.ua.err = err
		var re *oauth2.RetrieveError
		if errors.As(err, &re) && re.ErrorCode == "invalid_grant" {
			// Revoked or expired (7 days while the app is in "Testing").
			s.ua.client = nil
		}
		s.ua.mu.Unlock()
		return nil, err
	}
	if t.RefreshToken != "" && t.AccessToken != s.last {
		s.last = t.AccessToken
		_ = s.ua.store.Save(t)
	}
	return t, nil
}

func (ua *UserAuth) setToken(ctx context.Context, tok *oauth2.Token) {
	src := &savingSource{ua: ua, base: ua.cfg.TokenSource(context.Background(), tok)}
	ua.mu.Lock()
	ua.client = oauth2.NewClient(context.Background(), oauth2.ReuseTokenSource(tok, src))
	ua.err = nil
	ua.mu.Unlock()
}

// Client returns the signed-in client, or nil when Simon is not signed in.
func (ua *UserAuth) Client() *http.Client {
	if ua == nil {
		return nil
	}
	ua.mu.Lock()
	defer ua.mu.Unlock()
	return ua.client
}

// SignedIn reports whether a usable sign-in exists.
func (ua *UserAuth) SignedIn() bool { return ua.Client() != nil }

// LastError is the last token refresh error, if any.
func (ua *UserAuth) LastError() error {
	if ua == nil {
		return nil
	}
	ua.mu.Lock()
	defer ua.mu.Unlock()
	return ua.err
}

// SignIn runs the desktop OAuth flow: it opens the browser on Google's
// consent page and waits for the redirect to a local loopback port.
func (ua *UserAuth) SignIn(ctx context.Context, openBrowser func(string) error) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer ln.Close()
	cfg := *ua.cfg
	cfg.RedirectURL = fmt.Sprintf("http://127.0.0.1:%d/", ln.Addr().(*net.TCPAddr).Port)

	stateBytes := make([]byte, 16)
	rand.Read(stateBytes)
	state := hex.EncodeToString(stateBytes)
	verifier := oauth2.GenerateVerifier()

	type result struct {
		code string
		err  error
	}
	done := make(chan result, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != state {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		if e := q.Get("error"); e != "" {
			fmt.Fprintf(w, "Přihlášení se nepovedlo (%s). Okno můžeš zavřít.", e)
			done <- result{err: fmt.Errorf("sign-in refused: %s", e)}
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<p style='font-family:sans-serif'>Přihlášeno. Okno můžeš zavřít.</p>")
		done <- result{code: q.Get("code")}
	})}
	go srv.Serve(ln)
	defer srv.Close()

	authURL := cfg.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.ApprovalForce, oauth2.S256ChallengeOption(verifier))
	if err := openBrowser(authURL); err != nil {
		return fmt.Errorf("open browser: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	select {
	case <-ctx.Done():
		return errors.New("sign-in timed out")
	case r := <-done:
		if r.err != nil {
			return r.err
		}
		tok, err := cfg.Exchange(ctx, r.code, oauth2.VerifierOption(verifier))
		if err != nil {
			return fmt.Errorf("exchange code: %w", err)
		}
		if tok.RefreshToken == "" {
			return errors.New("google returned no refresh token; remove the app at myaccount.google.com/permissions and sign in again")
		}
		if err := ua.store.Save(tok); err != nil {
			return fmt.Errorf("save token: %w", err)
		}
		ua.cfg.RedirectURL = cfg.RedirectURL
		ua.setToken(ctx, tok)
		return nil
	}
}

// SignOut forgets the stored token.
func (ua *UserAuth) SignOut() {
	ua.mu.Lock()
	ua.client = nil
	ua.mu.Unlock()
	_ = ua.store.Delete()
}

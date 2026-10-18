// Package gdrive talks to the Google Drive REST API v3.
//
// Two identities are used (decision: Simon, 18 Oct 2026):
//   - the service account edits existing files and detects changes;
//   - Simon's own Google account (OAuth, desktop flow) creates new documents
//     and folders and moves files to the bin, because the service account has
//     no storage quota (files.create fails with 403 storageQuotaExceeded) and
//     cannot trash files it does not own.
//
// The endpoints and their behaviour were verified against the real API on
// 18 Oct 2026; see CLAUDE.md.
package gdrive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	MimeDoc    = "application/vnd.google-apps.document"
	MimeFolder = "application/vnd.google-apps.folder"

	apiBase    = "https://www.googleapis.com/drive/v3"
	uploadBase = "https://www.googleapis.com/upload/drive/v3"
	itemFields = "id,name,mimeType,parents,version,modifiedTime,trashed"
)

// Kind says how a Drive file maps to a local .md file.
type Kind int

const (
	KindOther Kind = iota
	KindDoc        // Google Doc: converted with the markdown export/import
	KindRaw        // a real .md file stored on Drive: bytes copied as they are
	KindFolder
)

// Item is the metadata of one Drive file or folder.
type Item struct {
	ID       string
	Name     string
	MimeType string
	Parents  []string
	Version  int64
	Modified time.Time
	Trashed  bool
}

// Kind classifies the item.
func (it Item) Kind() Kind {
	switch {
	case it.MimeType == MimeFolder:
		return KindFolder
	case it.MimeType == MimeDoc:
		return KindDoc
	case it.MimeType == "text/markdown" || it.MimeType == "text/x-markdown":
		return KindRaw
	case strings.HasSuffix(strings.ToLower(it.Name), ".md") && (it.MimeType == "text/plain" || it.MimeType == "application/octet-stream"):
		return KindRaw
	}
	return KindOther
}

type apiItem struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	MimeType     string   `json:"mimeType"`
	Parents      []string `json:"parents"`
	Version      string   `json:"version"`
	ModifiedTime string   `json:"modifiedTime"`
	Trashed      bool     `json:"trashed"`
}

func (a apiItem) item() Item {
	v, _ := strconv.ParseInt(a.Version, 10, 64)
	t, _ := time.Parse(time.RFC3339Nano, a.ModifiedTime)
	return Item{ID: a.ID, Name: a.Name, MimeType: a.MimeType, Parents: a.Parents, Version: v, Modified: t, Trashed: a.Trashed}
}

// Change is one entry of changes.list.
type Change struct {
	FileID  string
	Removed bool
	File    *Item
}

// ErrNotSignedIn is returned by operations that need Simon's own account
// (creating, moving to the bin) when he has not signed in yet.
var ErrNotSignedIn = errors.New("not signed in with Google (needed to create new notes on Drive)")

// APIError is a non-2xx answer from Google.
type APIError struct {
	Status  int
	Reason  string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("google api %d %s: %s", e.Status, e.Reason, e.Message)
}

// IsNotFound reports a 404.
func IsNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == 404
}

// IsRetryable reports errors worth retrying later: network problems, rate
// limits, server errors.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	var ae *APIError
	if errors.As(err, &ae) {
		switch {
		case ae.Status == 429 || ae.Status >= 500:
			return true
		case ae.Status == 403 && (ae.Reason == "rateLimitExceeded" || ae.Reason == "userRateLimitExceeded"):
			return true
		}
		return false
	}
	if errors.Is(err, ErrNotSignedIn) {
		return false
	}
	return true // network errors, timeouts
}

// Client is the Drive client. SA is the service account HTTP client, User is
// Simon's signed-in client (nil until he signs in).
type Client struct {
	SA   *http.Client
	User func() *http.Client
}

func (c *Client) user() (*http.Client, error) {
	if c.User == nil {
		return nil, ErrNotSignedIn
	}
	if u := c.User(); u != nil {
		return u, nil
	}
	return nil, ErrNotSignedIn
}

func do(ctx context.Context, hc *http.Client, method, u string, body io.Reader, ctype string, out any) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error struct {
				Message string `json:"message"`
				Errors  []struct {
					Reason string `json:"reason"`
				} `json:"errors"`
			} `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		ae := &APIError{Status: resp.StatusCode, Message: e.Error.Message}
		if len(e.Error.Errors) > 0 {
			ae.Reason = e.Error.Errors[0].Reason
		}
		if ae.Message == "" {
			ae.Message = strings.TrimSpace(string(data))
		}
		return nil, ae
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return nil, fmt.Errorf("decode %s: %w", u, err)
		}
	}
	return data, nil
}

// StartPageToken returns the token for "changes from now on".
func (c *Client) StartPageToken(ctx context.Context) (string, error) {
	var r struct {
		StartPageToken string `json:"startPageToken"`
	}
	_, err := do(ctx, c.SA, "GET", apiBase+"/changes/startPageToken?supportsAllDrives=true", nil, "", &r)
	return r.StartPageToken, err
}

// Changes lists everything that changed since token, among the files the
// service account can see. With nothing changed this is a single request.
func (c *Client) Changes(ctx context.Context, token string) ([]Change, string, error) {
	var all []Change
	for {
		q := url.Values{}
		q.Set("pageToken", token)
		q.Set("pageSize", "1000")
		q.Set("supportsAllDrives", "true")
		q.Set("includeItemsFromAllDrives", "true")
		q.Set("fields", "nextPageToken,newStartPageToken,changes(fileId,removed,file("+itemFields+"))")
		var r struct {
			NextPageToken     string `json:"nextPageToken"`
			NewStartPageToken string `json:"newStartPageToken"`
			Changes           []struct {
				FileID  string   `json:"fileId"`
				Removed bool     `json:"removed"`
				File    *apiItem `json:"file"`
			} `json:"changes"`
		}
		if _, err := do(ctx, c.SA, "GET", apiBase+"/changes?"+q.Encode(), nil, "", &r); err != nil {
			return nil, "", err
		}
		for _, ch := range r.Changes {
			x := Change{FileID: ch.FileID, Removed: ch.Removed}
			if ch.File != nil {
				it := ch.File.item()
				x.File = &it
			}
			all = append(all, x)
		}
		if r.NextPageToken == "" {
			return all, r.NewStartPageToken, nil
		}
		token = r.NextPageToken
	}
}

// ListFolder lists the non-trashed children of a folder.
func (c *Client) ListFolder(ctx context.Context, folderID string) ([]Item, error) {
	var out []Item
	page := ""
	for {
		q := url.Values{}
		q.Set("q", fmt.Sprintf("'%s' in parents and trashed = false", strings.ReplaceAll(folderID, "'", `\'`)))
		q.Set("pageSize", "1000")
		q.Set("supportsAllDrives", "true")
		q.Set("includeItemsFromAllDrives", "true")
		q.Set("fields", "nextPageToken,files("+itemFields+")")
		if page != "" {
			q.Set("pageToken", page)
		}
		var r struct {
			NextPageToken string    `json:"nextPageToken"`
			Files         []apiItem `json:"files"`
		}
		if _, err := do(ctx, c.SA, "GET", apiBase+"/files?"+q.Encode(), nil, "", &r); err != nil {
			return nil, err
		}
		for _, f := range r.Files {
			out = append(out, f.item())
		}
		if r.NextPageToken == "" {
			return out, nil
		}
		page = r.NextPageToken
	}
}

// Get returns one item's metadata.
func (c *Client) Get(ctx context.Context, id string) (Item, error) {
	var a apiItem
	_, err := do(ctx, c.SA, "GET", apiBase+"/files/"+url.PathEscape(id)+"?supportsAllDrives=true&fields="+itemFields, nil, "", &a)
	return a.item(), err
}

// Download returns the file's content as markdown: the markdown export for a
// Google Doc, the bytes for a raw .md file.
func (c *Client) Download(ctx context.Context, it Item) (string, error) {
	var u string
	switch it.Kind() {
	case KindDoc:
		u = apiBase + "/files/" + url.PathEscape(it.ID) + "/export?mimeType=text/markdown"
	case KindRaw:
		u = apiBase + "/files/" + url.PathEscape(it.ID) + "?alt=media&supportsAllDrives=true"
	default:
		return "", fmt.Errorf("%s (%s) is not a markdown document", it.Name, it.MimeType)
	}
	data, err := do(ctx, c.SA, "GET", u, nil, "", nil)
	return string(data), err
}

// Upload replaces the file's content. For a Google Doc, Docs converts the
// markdown into formatting. Returns the new metadata (with the new version).
func (c *Client) Upload(ctx context.Context, it Item, content string) (Item, error) {
	ctype := "text/markdown; charset=utf-8"
	if it.Kind() == KindRaw {
		ctype = it.MimeType
	}
	var a apiItem
	u := uploadBase + "/files/" + url.PathEscape(it.ID) + "?uploadType=media&supportsAllDrives=true&fields=" + itemFields
	_, err := do(ctx, c.SA, "PATCH", u, strings.NewReader(content), ctype, &a)
	return a.item(), err
}

// Rename changes the name and, when newParent differs from oldParent, moves
// the file. Moving uses Simon's account when signed in.
func (c *Client) Rename(ctx context.Context, id, name, oldParent, newParent string) (Item, error) {
	hc := c.SA
	q := url.Values{}
	q.Set("supportsAllDrives", "true")
	q.Set("fields", itemFields)
	if newParent != "" && newParent != oldParent {
		q.Set("addParents", newParent)
		q.Set("removeParents", oldParent)
		if u, err := c.user(); err == nil {
			hc = u
		}
	}
	body, _ := json.Marshal(map[string]string{"name": name})
	var a apiItem
	_, err := do(ctx, hc, "PATCH", apiBase+"/files/"+url.PathEscape(id)+"?"+q.Encode(), bytes.NewReader(body), "application/json", &a)
	return a.item(), err
}

// CreateDoc creates a Google Doc from markdown in parent, as Simon.
func (c *Client) CreateDoc(ctx context.Context, parent, name, content string) (Item, error) {
	hc, err := c.user()
	if err != nil {
		return Item{}, err
	}
	meta, _ := json.Marshal(map[string]any{"name": name, "mimeType": MimeDoc, "parents": []string{parent}})
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	h := textproto.MIMEHeader{}
	h.Set("Content-Type", "application/json; charset=UTF-8")
	pw, _ := mw.CreatePart(h)
	pw.Write(meta)
	h = textproto.MIMEHeader{}
	h.Set("Content-Type", "text/markdown; charset=utf-8")
	pw, _ = mw.CreatePart(h)
	pw.Write([]byte(content))
	mw.Close()
	var a apiItem
	u := uploadBase + "/files?uploadType=multipart&supportsAllDrives=true&fields=" + itemFields
	_, err = do(ctx, hc, "POST", u, &buf, "multipart/related; boundary="+mw.Boundary(), &a)
	return a.item(), err
}

// CreateFolder creates a folder in parent, as Simon.
func (c *Client) CreateFolder(ctx context.Context, parent, name string) (Item, error) {
	hc, err := c.user()
	if err != nil {
		return Item{}, err
	}
	body, _ := json.Marshal(map[string]any{"name": name, "mimeType": MimeFolder, "parents": []string{parent}})
	var a apiItem
	_, err = do(ctx, hc, "POST", apiBase+"/files?supportsAllDrives=true&fields="+itemFields, bytes.NewReader(body), "application/json", &a)
	return a.item(), err
}

// Trash moves a file to the Drive bin, as Simon (only the owner can).
func (c *Client) Trash(ctx context.Context, id string) error {
	hc, err := c.user()
	if err != nil {
		return err
	}
	body := strings.NewReader(`{"trashed":true}`)
	_, err = do(ctx, hc, "PATCH", apiBase+"/files/"+url.PathEscape(id)+"?supportsAllDrives=true&fields=id", body, "application/json", nil)
	return err
}

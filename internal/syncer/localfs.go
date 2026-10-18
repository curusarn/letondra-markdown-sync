package syncer

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/curusarn/letondra-markdown-sync/internal/gdrive"
)

// localFile is a .md file found in a local mapping folder.
type localFile struct {
	Rel   string // slash-separated path relative to the mapping root
	Size  int64
	Mtime int64
}

const tmpPrefix = ".lms-tmp-"

// isIgnoredName reports names that are never synced: hidden files and
// folders (.obsidian, .trash, .git), our temp files.
func isIgnoredName(name string) bool {
	return strings.HasPrefix(name, ".") || strings.HasPrefix(name, "~$")
}

// excluded matches a relative path against the mapping's exclude patterns.
// "Archiv" or "Archiv/**" excludes the folder, "*.draft.md" matches names.
func excluded(rel string, patterns []string) bool {
	for _, p := range patterns {
		p = strings.TrimSuffix(strings.ReplaceAll(strings.TrimSpace(p), `\`, "/"), "/**")
		p = strings.TrimSuffix(p, "/")
		if p == "" {
			continue
		}
		if rel == p || strings.HasPrefix(rel, p+"/") {
			return true
		}
		if ok, _ := path.Match(p, rel); ok {
			return true
		}
		if ok, _ := path.Match(p, path.Base(rel)); ok {
			return true
		}
	}
	return false
}

// walkLocal lists the .md files under root.
func walkLocal(root string, exclude []string) (map[string]localFile, error) {
	out := map[string]localFile{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root {
				return err
			}
			return nil // unreadable subfolder: skip it, keep going
		}
		if p == root {
			return nil
		}
		rel := filepath.ToSlash(strings.TrimPrefix(p, root+string(filepath.Separator)))
		if isIgnoredName(d.Name()) || excluded(rel, exclude) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || !strings.EqualFold(filepath.Ext(d.Name()), ".md") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		out[rel] = localFile{Rel: rel, Size: info.Size(), Mtime: info.ModTime().UnixNano()}
		return nil
	})
	return out, err
}

func absPath(root, rel string) string { return filepath.Join(root, filepath.FromSlash(rel)) }

// readLocal reads a local file as text with LF line endings kept as they are.
func readLocal(root, rel string) (string, localFile, error) {
	p := absPath(root, rel)
	data, err := os.ReadFile(p)
	if err != nil {
		return "", localFile{}, err
	}
	info, err := os.Stat(p)
	if err != nil {
		return "", localFile{}, err
	}
	return string(data), localFile{Rel: rel, Size: info.Size(), Mtime: info.ModTime().UnixNano()}, nil
}

// writeLocal writes atomically: a temp file in the same folder, then a
// rename over the target. Obsidian sees one change and reloads the note.
// Windows may refuse the rename while another program holds the file, so it
// retries a few times.
func writeLocal(root, rel, text string) (localFile, error) {
	p := absPath(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return localFile{}, err
	}
	tmp := filepath.Join(filepath.Dir(p), tmpPrefix+filepath.Base(p))
	if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
		return localFile{}, err
	}
	var err error
	for i := 0; i < 10; i++ {
		if err = os.Rename(tmp, p); err == nil {
			break
		}
		time.Sleep(time.Duration(100*(i+1)) * time.Millisecond)
	}
	if err != nil {
		os.Remove(tmp)
		return localFile{}, fmt.Errorf("write %s: %w", p, err)
	}
	info, err := os.Stat(p)
	if err != nil {
		return localFile{}, err
	}
	return localFile{Rel: rel, Size: info.Size(), Mtime: info.ModTime().UnixNano()}, nil
}

// backup copies a local file into the app's backups folder before it is
// overwritten or removed. Backups older than 30 days are deleted at start.
func (e *Engine) backup(root, rel, why string) {
	data, err := os.ReadFile(absPath(root, rel))
	if err != nil {
		return
	}
	now := e.now()
	dst := filepath.Join(e.dir, "backups", now.Format("2006-01-02"), filepath.Base(root), filepath.FromSlash(rel)+"."+now.Format("150405")+"."+why+".md")
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err == nil {
		_ = os.WriteFile(dst, data, 0o600)
	}
}

func (e *Engine) pruneBackups() {
	dir := filepath.Join(e.dir, "backups")
	entries, _ := os.ReadDir(dir)
	cutoff := e.now().AddDate(0, 0, -30).Format("2006-01-02")
	for _, d := range entries {
		if d.IsDir() && d.Name() < cutoff {
			os.RemoveAll(filepath.Join(dir, d.Name()))
		}
	}
}

// moveLocal renames a local file, creating folders as needed. It refuses to
// overwrite an existing file.
func moveLocal(root, from, to string) error {
	src, dst := absPath(root, from), absPath(root, to)
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("%s already exists", dst)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.Rename(src, dst)
}

// removeLocal moves a file to the backups folder (never a hard delete).
func (e *Engine) removeLocal(root, rel string) error {
	e.backup(root, rel, "deleted-on-drive")
	err := os.Remove(absPath(root, rel))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// windowsReserved are names Windows refuses as file names.
var windowsReserved = map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true}

// safeName turns a Drive name into a name Windows accepts.
func safeName(name string) string {
	r := strings.NewReplacer(`<`, "‹", `>`, "›", `:`, "꞉", `"`, "”", `/`, "∕", `\`, "∖", `|`, "ǀ", `?`, "？", `*`, "∗")
	name = r.Replace(name)
	name = strings.Map(func(c rune) rune {
		if c < 32 {
			return -1
		}
		return c
	}, name)
	name = strings.TrimRight(name, ". ")
	if name == "" {
		name = "Bez názvu"
	}
	if windowsReserved[strings.ToUpper(strings.SplitN(name, ".", 2)[0])] {
		name += "_"
	}
	return name
}

// localName is the local file name for a Drive document.
func localName(it gdrive.Item) string {
	n := safeName(it.Name)
	if !strings.HasSuffix(strings.ToLower(n), ".md") {
		n += ".md"
	}
	return n
}

// driveName is the Drive document name for a local file: the file name
// without .md. Raw .md files on Drive keep the extension.
func driveName(rel string, kind gdrive.Kind) string {
	base := path.Base(rel)
	if kind == gdrive.KindRaw {
		return base
	}
	return strings.TrimSuffix(base, path.Ext(base))
}

// uniqueRel returns rel, or rel with " (2)", " (3)"… when taken (case
// insensitive, as on Windows).
func uniqueRel(rel string, taken func(string) bool) string {
	if !taken(strings.ToLower(rel)) {
		return rel
	}
	ext := path.Ext(rel)
	stem := strings.TrimSuffix(rel, ext)
	for i := 2; ; i++ {
		c := fmt.Sprintf("%s (%d)%s", stem, i, ext)
		if !taken(strings.ToLower(c)) {
			return c
		}
	}
}

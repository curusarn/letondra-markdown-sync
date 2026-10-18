// Package logx writes the app log to a file in the app folder, rotated at
// 5 MB (one old file kept as app.log.1).
package logx

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
)

type rotating struct {
	mu   sync.Mutex
	path string
	f    *os.File
	size int64
}

func (r *rotating) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil || r.size > 5<<20 {
		if r.f != nil {
			r.f.Close()
			os.Rename(r.path, r.path+".1")
		}
		f, err := os.OpenFile(r.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return 0, err
		}
		info, _ := f.Stat()
		r.f, r.size = f, info.Size()
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// Path is the log file path in dir.
func Path(dir string) string { return filepath.Join(dir, "app.log") }

// New returns a logger writing to dir/app.log (and to stderr when extra is
// set, for running from a console).
func New(dir string, extra io.Writer) *log.Logger {
	var w io.Writer = &rotating{path: Path(dir)}
	if extra != nil {
		w = io.MultiWriter(w, extra)
	}
	return log.New(w, "", log.LstdFlags)
}

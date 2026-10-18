// Package watch reports changes in a folder tree.
package watch

import (
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/fsnotify/fsnotify"
)

// Tree watches root and all its subfolders (except hidden ones like
// .obsidian) and calls onChange for every event. New subfolders are added as
// they appear. The engine debounces and re-reads the folder, so lost or
// duplicate events do no harm.
func Tree(root string, onChange func(), onError func(error)) (stop func(), err error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	add := func(dir string) {
		filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || !d.IsDir() {
				return nil
			}
			if p != dir && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			if err := w.Add(p); err != nil && onError != nil {
				onError(err)
			}
			return nil
		})
	}
	add(root)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				if ev.Op&fsnotify.Create != 0 {
					add(ev.Name) // a new folder: watch it too (no-op for files)
				}
				onChange()
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				if onError != nil {
					onError(err)
				}
				onChange() // events may have been lost: let the engine re-read
			}
		}
	}()
	return func() { close(done); w.Close() }, nil
}

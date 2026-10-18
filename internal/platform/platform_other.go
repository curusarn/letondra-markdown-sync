//go:build !windows

package platform

import (
	"os/exec"
	"runtime"
)

// IdleSeconds is not implemented outside Windows (development only).
func IdleSeconds() int { return 0 }

// OpenURL opens a URL with the system handler.
func OpenURL(u string) error {
	if runtime.GOOS == "darwin" {
		return exec.Command("open", u).Start()
	}
	return exec.Command("xdg-open", u).Start()
}

// OpenFile opens a file or folder.
func OpenFile(p string) error { return OpenURL(p) }

func startProgram(p string) error { return exec.Command(p).Start() }

// Protect is a no-op outside Windows; the token file is only readable by the
// user (0600).
func Protect(data []byte) ([]byte, error) { return data, nil }

// Unprotect is a no-op outside Windows.
func Unprotect(data []byte) ([]byte, error) { return data, nil }

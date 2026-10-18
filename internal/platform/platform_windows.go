//go:build windows

package platform

import (
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32           = windows.NewLazySystemDLL("user32.dll")
	kernel32         = windows.NewLazySystemDLL("kernel32.dll")
	procLastInput    = user32.NewProc("GetLastInputInfo")
	procGetTickCount = kernel32.NewProc("GetTickCount")
)

type lastInputInfo struct {
	cbSize uint32
	dwTime uint32
}

// IdleSeconds is how long there has been no keyboard or mouse input.
func IdleSeconds() int {
	li := lastInputInfo{cbSize: uint32(unsafe.Sizeof(lastInputInfo{}))}
	if r, _, _ := procLastInput.Call(uintptr(unsafe.Pointer(&li))); r == 0 {
		return 0
	}
	now, _, _ := procGetTickCount.Call()
	return int((uint32(now) - li.dwTime) / 1000)
}

// OpenURL opens a URL (web page or obsidian://) with its default handler.
func OpenURL(u string) error {
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Start()
}

// OpenFile opens a file or folder in its default app (Explorer, Notepad).
func OpenFile(p string) error { return OpenURL(p) }

func startProgram(p string) error {
	return exec.Command(p).Start()
}

// Protect encrypts data for the current Windows user (DPAPI), so the stored
// Google token is useless if the file is copied elsewhere.
func Protect(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return data, nil
	}
	in := windows.DataBlob{Size: uint32(len(data)), Data: &data[0]}
	var out windows.DataBlob
	if err := windows.CryptProtectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil
}

// Unprotect reverses Protect.
func Unprotect(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return data, nil
	}
	in := windows.DataBlob{Size: uint32(len(data)), Data: &data[0]}
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil
}

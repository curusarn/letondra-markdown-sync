//go:build !windows

package tray

import (
	"os"
	"os/signal"
)

// Actions are the menu callbacks.
type Actions struct {
	Open, SyncNow, SignIn, OpenLog, Quit func()
}

var stop = make(chan struct{})

// Run has no tray outside Windows (development): it blocks until Ctrl+C.
func Run(a Actions, ready func()) {
	ready()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	select {
	case <-sig:
	case <-stop:
	}
	a.Quit()
}

// Set is a no-op without a tray.
func Set(state, msg string) {}

// Stop ends Run.
func Stop() {
	select {
	case <-stop:
	default:
		close(stop)
	}
}

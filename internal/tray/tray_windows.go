//go:build windows

package tray

import (
	"fyne.io/systray"
)

// Actions are the menu callbacks.
type Actions struct {
	Open, SyncNow, SignIn, OpenLog, Quit func()
}

var status *systray.MenuItem

// Run shows the tray icon and blocks until Quit.
func Run(a Actions, ready func()) {
	systray.Run(func() {
		systray.SetIcon(Icon("syncing"))
		systray.SetTitle("Markdown Sync")
		systray.SetTooltip("Markdown Sync")
		status = systray.AddMenuItem("Spouštím…", "")
		status.Disable()
		systray.AddSeparator()
		open := systray.AddMenuItem("Otevřít přehled", "")
		sync := systray.AddMenuItem("Synchronizovat teď", "")
		signin := systray.AddMenuItem("Přihlásit se Googlem", "")
		logs := systray.AddMenuItem("Otevřít log", "")
		systray.AddSeparator()
		quit := systray.AddMenuItem("Ukončit", "")
		systray.SetOnTapped(a.Open)
		go func() {
			for {
				select {
				case <-open.ClickedCh:
					a.Open()
				case <-sync.ClickedCh:
					a.SyncNow()
				case <-signin.ClickedCh:
					a.SignIn()
				case <-logs.ClickedCh:
					a.OpenLog()
				case <-quit.ClickedCh:
					a.Quit()
					systray.Quit()
					return
				}
			}
		}()
		ready()
	}, func() {})
}

// Set updates the icon and the status line.
func Set(state, msg string) {
	if status == nil {
		return
	}
	systray.SetIcon(Icon(state))
	tip := "Markdown Sync: " + msg
	if len(tip) > 120 {
		tip = tip[:120]
	}
	systray.SetTooltip(tip)
	status.SetTitle(msg)
}

// Stop removes the icon.
func Stop() { systray.Quit() }

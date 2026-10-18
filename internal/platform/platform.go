// Package platform holds the bits that differ between Windows (the target)
// and other systems (development and tests on a Mac).
package platform

import (
	"strings"
)

// Launch starts the configured program: a URL ("obsidian://open") goes to
// its registered handler, anything else is run as a program path.
func Launch(target string) error {
	target = strings.TrimSpace(target)
	if target == "" {
		return nil
	}
	if strings.Contains(target, "://") {
		return OpenURL(target)
	}
	return startProgram(target)
}

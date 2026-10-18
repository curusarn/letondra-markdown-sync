//go:build windows

package platform

import (
	"bytes"
	"testing"
)

func TestDPAPIRoundTrip(t *testing.T) {
	in := []byte(`{"refresh_token":"x"}`)
	enc, err := Protect(in)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(enc, []byte("refresh_token")) {
		t.Fatal("not encrypted")
	}
	out, err := Unprotect(enc)
	if err != nil || !bytes.Equal(out, in) {
		t.Fatalf("round trip: %q %v", out, err)
	}
}

func TestIdleSeconds(t *testing.T) {
	if s := IdleSeconds(); s < 0 {
		t.Fatalf("idle %d", s)
	}
}

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// isolateActiveAuth points the on-disk selection at a temp file.
func isolateActiveAuth(t *testing.T) {
	t.Helper()
	old := activeAuthStatePath
	activeAuthStatePath = filepath.Join(t.TempDir(), "active-auth")
	resetActiveAuthState()
	t.Cleanup(func() {
		resetActiveAuthState()
		activeAuthStatePath = old
	})
}

// TestActiveAuth_PersistsAcrossRestart simulates a service restart: select an
// account, drop all memory state, and verify the selection is restored from
// disk instead of falling back to the first card.
func TestActiveAuth_PersistsAcrossRestart(t *testing.T) {
	isolateActiveAuth(t)
	setActiveAuthID("wb-second")
	if got := getActiveAuthID(); got != "wb-second" {
		t.Fatalf("got %q, want wb-second", got)
	}
	// Simulate restart: wipe memory, keep the file.
	activeAuthMu.Lock()
	activeAuthID = ""
	activeAuthLoaded = false
	activeAuthMu.Unlock()

	if got := getActiveAuthID(); got != "wb-second" {
		t.Fatalf("after restart got %q, want wb-second", got)
	}
}

// TestActiveAuth_ClearRemovesFile verifies clearing the selection (account
// deleted) also drops the backing file so a restart falls back to first.
func TestActiveAuth_ClearRemovesFile(t *testing.T) {
	isolateActiveAuth(t)
	setActiveAuthID("wb-gone")
	clearActiveAuthIfMatch("wb-gone")
	if got := getActiveAuthID(); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
	if _, err := os.Stat(activeAuthStatePath); !os.IsNotExist(err) {
		t.Fatal("state file should be removed after clear")
	}
}

// TestActiveAuth_MissingFileFallsBack verifies a fresh machine (no file)
// behaves as before: empty selection → first usable card.
func TestActiveAuth_MissingFileFallsBack(t *testing.T) {
	isolateActiveAuth(t)
	if got := getActiveAuthID(); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

//go:build !windows

package main

import "testing"

// vi ships on every stock unix; it is the traditional $EDITOR fallback and
// needs no probing for what else might be installed.
func TestDefaultEditorIsViOnUnix(t *testing.T) {
	if got := defaultEditor(); got != "vi" {
		t.Errorf("defaultEditor() = %q, want vi", got)
	}
}

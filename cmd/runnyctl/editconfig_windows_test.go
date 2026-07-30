//go:build windows

package main

import "testing"

// notepad.exe is always present on a stock Windows install, unlike vi — the
// unix fallback would leave anyone without $EDITOR/$VISUAL set unable to run
// edit-config at all.
func TestDefaultEditorIsNotepadOnWindows(t *testing.T) {
	if got := defaultEditor(); got != "notepad.exe" {
		t.Errorf("defaultEditor() = %q, want notepad.exe", got)
	}
}

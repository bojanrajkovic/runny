//go:build windows

package main

// defaultEditor is openEditor's fallback when neither $VISUAL nor $EDITOR is
// set. vi does not exist on a stock Windows host, so the recommended
// edit-config path would fail outright; notepad.exe is always present.
func defaultEditor() string { return "notepad.exe" }

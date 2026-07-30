//go:build !windows

package main

// defaultEditor is openEditor's fallback when neither $VISUAL nor $EDITOR is
// set: the traditional unix default, present on every stock install.
func defaultEditor() string { return "vi" }

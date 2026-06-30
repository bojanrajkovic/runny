package statemachine

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// ansiRE matches ANSI/VT escape sequences found in script(1) recordings:
// CSI sequences (ESC [ ... letter), OSC sequences (ESC ] ... BEL or ST), and
// standalone ESC sequences. OSC is required for macOS Terminal, which emits
// window-title sequences (ESC ] 0 ; title BEL) on every prompt.
var ansiRE = regexp.MustCompile(`\x1b(?:\[[0-9;?]*[A-Za-z]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[^[\]])`)

// stripTerminalCodes removes ANSI escape sequences and normalizes the \r\n
// line endings that PTY/script output produces.
func stripTerminalCodes(b []byte) []byte {
	b = ansiRE.ReplaceAll(b, nil)
	return bytes.ReplaceAll(b, []byte{'\r', '\n'}, []byte{'\n'})
}

// Small fs helpers kept separate so fsm.go stays pure control flow.

func writeFile(dir, name string, data []byte) error {
	// 0o600: post-mortem artifacts include runner _diag tails, which can
	// carry unmasked job secrets on verbose runs.
	return os.WriteFile(filepath.Join(dir, name), data, 0o600)
}

// cloneRunnerTarball CoW-clones this cycle's resolved runner tarball (basename
// tarball) from the shared download store into the slot's own mount dir, which
// is then mounted read-only into the guest. The cycle owns the clone end to
// end: no concurrent slot or store GC can pull it out from under the guest, and
// the mount holds exactly one tarball. 0o700 mount dir — the tree is owner-only
// throughout (it sits under the owner-only vms/).
func cloneRunnerTarball(cloneFile FileCloner, storeDir, mountDir, tarball string) error {
	if err := os.MkdirAll(mountDir, 0o700); err != nil {
		return fmt.Errorf("creating runner mount dir: %w", err)
	}
	return cloneFile(filepath.Join(storeDir, tarball), filepath.Join(mountDir, tarball))
}

// removeAll is a var so teardown's best-effort clone deletion can be made to
// fail in tests (the cleanup-failure-is-recorded path).
var removeAll = func(path string) error {
	if path == "" {
		return nil
	}
	return os.RemoveAll(path)
}

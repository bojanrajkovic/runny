package statemachine

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// ansiRE matches ANSI/VT escape sequences found in script(1) recordings:
//   - CSI  ESC [ params letter        (color, cursor movement, erase, …)
//   - OSC  ESC ] … BEL|ST            (window titles — macOS Terminal emits these on every prompt)
//   - Designator  ESC ( ) * + final  (3-byte character-set sequences; ncurses uses ESC(B and ESC(0)
//   - Simple  ESC <any other byte>    (ESC M reverse-index, ESC = alt-keypad, …)
var ansiRE = regexp.MustCompile(`\x1b(?:\[[0-9;?]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[()*+][0-9A-Za-z]|[^[\]()*+])`)

// stripTerminalCodes removes ANSI/VT escape sequences and all carriage returns
// from PTY/script output, yielding plain text. Stripping \r handles both
// \r\n line endings (common in PTY recordings) and bare \r cursor-resets
// (progress bars, npm, curl) in one pass.
func stripTerminalCodes(b []byte) []byte {
	b = ansiRE.ReplaceAll(b, nil)
	return bytes.ReplaceAll(b, []byte{'\r'}, nil)
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

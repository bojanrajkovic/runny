package statemachine

import (
	"os"
	"path/filepath"
)

// Small fs helpers kept separate so fsm.go stays pure control flow.

func writeFile(dir, name string, data []byte) error {
	// 0o600: post-mortem artifacts include runner _diag tails, which can
	// carry unmasked job secrets on verbose runs.
	return os.WriteFile(filepath.Join(dir, name), data, 0o600)
}

func removeAll(path string) error {
	if path == "" {
		return nil
	}
	return os.RemoveAll(path)
}

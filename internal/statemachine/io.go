package statemachine

import (
	"os"
	"path/filepath"
)

// Small fs helpers kept separate so fsm.go stays pure control flow.

func writeFile(dir, name string, data []byte) error {
	return os.WriteFile(filepath.Join(dir, name), data, 0o644)
}

func removeAll(path string) error {
	if path == "" {
		return nil
	}
	return os.RemoveAll(path)
}

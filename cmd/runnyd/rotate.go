package main

import (
	"os"
	"sync"
)

// logFileCap bounds runnyd.log before rotation. Debug-level and append-only,
// the log once grew without bound on the same disk whose headroom the daemon
// itself guards.
const logFileCap = 100 << 20

// rotatingFile is a size-capped append writer: when a write would push the
// file past cap, the current file is renamed to <path>.1 (replacing the
// previous generation) and a fresh file is opened. One generation of history
// is enough for a post-mortem; cycle records carry the long-term story.
type rotatingFile struct {
	path string
	cap  int64

	mu   sync.Mutex
	f    *os.File
	size int64
}

func openRotatingFile(path string, capBytes int64) (*rotatingFile, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &rotatingFile{path: path, cap: capBytes, f: f, size: st.Size()}, nil
}

func (r *rotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size > 0 && r.size+int64(len(p)) > r.cap {
		_ = r.f.Close()
		_ = os.Rename(r.path, r.path+".1")
		f, err := os.OpenFile(r.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return 0, err
		}
		r.f, r.size = f, 0
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

func (r *rotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.f.Close()
}

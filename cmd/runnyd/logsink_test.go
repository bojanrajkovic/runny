package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// -doctor must not bring the log file into existence. Against another
// deployment's home the create is the read-only violation; against the
// invoker's own it is a second, UNLOCKED writer on a live daemon's log.
func TestOpenLogSinkDoctorOpensNoFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runnyd.log")
	var console bytes.Buffer

	w, closer, err := openLogSink(true, launchForeground, path, &console)
	if err != nil {
		t.Fatalf("openLogSink(-doctor) err = %v, want nil", err)
	}
	defer closer.Close()

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("openLogSink(-doctor) created %s (stat err = %v), want no file at all", path, err)
	}
	if _, err := w.Write([]byte("check output\n")); err != nil {
		t.Fatalf("writing to the doctor sink: %v", err)
	}
	if console.String() != "check output\n" {
		t.Errorf("doctor sink wrote %q to the console, want the line teed there", console.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a write through the doctor sink created %s, want the console only", path)
	}
}

// The real daemon still gets its file, and a foreground one still tees.
func TestOpenLogSinkDaemonOpensTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runnyd.log")
	var console bytes.Buffer

	w, closer, err := openLogSink(false, launchForeground, path, &console)
	if err != nil {
		t.Fatalf("openLogSink(daemon) err = %v, want nil", err)
	}
	if _, err := w.Write([]byte("runnyd starting\n")); err != nil {
		t.Fatalf("writing to the daemon sink: %v", err)
	}
	if err := closer.Close(); err != nil {
		t.Fatalf("closing the daemon sink: %v", err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if string(b) != "runnyd starting\n" {
		t.Errorf("%s holds %q, want the line written to it", path, b)
	}
	if console.String() != "runnyd starting\n" {
		t.Errorf("a foreground daemon wrote %q to the console, want the line teed there too", console.String())
	}
}

// A launchd/SCM-started daemon writes to the file only — the existing
// logSinkFor rule, still reached through the new seam.
func TestOpenLogSinkServiceDoesNotTee(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runnyd.log")
	var console bytes.Buffer

	w, closer, err := openLogSink(false, launchService, path, &console)
	if err != nil {
		t.Fatalf("openLogSink(service) err = %v, want nil", err)
	}
	defer closer.Close()

	if _, err := w.Write([]byte("runnyd starting\n")); err != nil {
		t.Fatalf("writing to the service sink: %v", err)
	}
	if console.String() != "" {
		t.Errorf("a service-started daemon wrote %q to the console, want the file only", console.String())
	}
}

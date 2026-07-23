package socket

import (
	"fmt"
	"os"
	"runtime/pprof"
	"testing"
	"time"
)

// TestMain installs a suite watchdog. A hung test on the windows CI runner is
// otherwise SIGKILLed by bazel at its 300s timeout with no Go stack dump, which
// tells us nothing. This fires first, well under that, dumping every goroutine
// stack to stderr (visible in the CI log) so the blocked test and its stack are
// identifiable. The darwin suite finishes in a few seconds, so it never fires
// there. 60s is far above the longest legitimately-bounded test (~5s) and far
// below bazel's kill.
func TestMain(m *testing.M) {
	go func() {
		time.Sleep(60 * time.Second)
		fmt.Fprintln(os.Stderr, "socket_test watchdog: suite exceeded 60s — a test is blocked; dumping goroutines")
		_ = pprof.Lookup("goroutine").WriteTo(os.Stderr, 2)
		os.Exit(2)
	}()
	os.Exit(m.Run())
}

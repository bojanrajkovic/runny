package guest

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestStopRunnerShellLogic runs the real stopRunnerScript through /bin/sh with
// a temp PATH that stubs pgrep/pkill/sleep, proving the three exit paths:
// exit 0 (listener dead), exit 1 (survived SIGKILL), exit 2 (pgrep tool failure).
func TestStopRunnerShellLogic(t *testing.T) {
	noop := "#!/bin/sh\nexit 0\n"
	tests := []struct {
		name     string
		pgrepSh  string
		wantCode int
	}{
		{
			name: "runner dies after a few polls",
			// Alive for the first 3 pgrep calls, then dead.
			pgrepSh: `#!/bin/sh
n=$(cat "$PGREP_COUNTER_FILE" 2>/dev/null || echo 0)
n=$((n+1))
echo "$n" > "$PGREP_COUNTER_FILE"
[ "$n" -le 3 ]
`,
			wantCode: 0,
		},
		{
			name:     "runner never dies",
			pgrepSh:  noop, // always alive → i reaches >24 → exit 1
			wantCode: 1,
		},
		{
			name:     "pgrep tool failure",
			pgrepSh:  "#!/bin/sh\nexit 2\n",
			wantCode: 2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			write := func(name, content string) {
				p := filepath.Join(dir, name)
				if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			write("pgrep", tc.pgrepSh)
			write("pkill", noop)
			write("sleep", noop)

			cmd := exec.Command("/bin/sh", "-c", stopRunnerScript)
			cmd.Env = []string{
				"PATH=" + dir + ":/usr/bin:/bin",
				"PGREP_COUNTER_FILE=" + filepath.Join(dir, "pgrep_counter"),
			}

			err := cmd.Run()
			got := 0
			if err != nil {
				exit, ok := err.(*exec.ExitError)
				if !ok {
					t.Fatalf("unexpected exec error: %v", err)
				}
				got = exit.ExitCode()
			}
			if got != tc.wantCode {
				t.Errorf("exit code = %d, want %d", got, tc.wantCode)
			}
		})
	}
}

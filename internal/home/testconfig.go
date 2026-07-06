package home

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// Verdict is the machine-readable result of `runnyd -test-config`: the
// cross-language contract the Swift app and runnyctl both parse to gate a
// daemon update. Status is ok|warn|error; errors and warnings are always
// arrays (never null). The schema is stable surface, versioned with the
// daemon: an older parser must keep reading a newer producer's output, so the
// JSON keys and the verdict strings below stay byte-identical across binary
// versions.
type Verdict struct {
	Status   string    `json:"status"`
	Errors   []string  `json:"errors"`
	Warnings []Warning `json:"warnings"`
}

// Verdict statuses. Contract surface (part of the -test-config JSON the Swift
// app and runnyctl both parse) — stable across config-schema revisions.
const (
	VerdictOK    = "ok"
	VerdictWarn  = "warn"
	VerdictError = "error"
)

// RunTestConfig execs `<binPath> -test-config <configPath>` and parses its
// JSON verdict from stdout. The exit code mirrors the verdict status
// (non-zero on error) but the JSON is the contract and is printed in every
// case, so the verdict is parsed from stdout regardless of the exit code;
// only a missing binary or unparseable output is a hard error.
func RunTestConfig(ctx context.Context, binPath, configPath string) (Verdict, error) {
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, binPath, "-test-config", configPath)
	cmd.Stdout, cmd.Stderr = &out, &errb
	runErr := cmd.Run()
	var v Verdict
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &v); err != nil {
		if detail := strings.TrimSpace(errb.String()); runErr != nil && detail != "" {
			return Verdict{}, fmt.Errorf("running %s -test-config: %v: %s", binPath, runErr, detail)
		} else if runErr != nil {
			return Verdict{}, fmt.Errorf("running %s -test-config: %w", binPath, runErr)
		}
		return Verdict{}, fmt.Errorf("%s -test-config produced no parseable verdict", binPath)
	}
	return v, nil
}

// Package testconfig execs a runnyd binary's `-test-config` and parses its
// verdict. Split out of internal/home (which the job core imports as a leaf
// package) because this is the one os/exec-shaped API in that surface, and
// none of home's other callers need it.
package testconfig

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/bojanrajkovic/runny/internal/home"
)

// RunTestConfig execs `<binPath> -test-config <configPath>` and parses its
// JSON verdict from stdout. The exit code mirrors the verdict status
// (non-zero on error) but the JSON is the contract and is printed in every
// case, so the verdict is parsed from stdout regardless of the exit code;
// only a missing binary or unparseable output is a hard error.
func RunTestConfig(ctx context.Context, binPath, configPath string) (home.Verdict, error) {
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, binPath, "-test-config", configPath)
	cmd.Stdout, cmd.Stderr = &out, &errb
	runErr := cmd.Run()
	var v home.Verdict
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &v); err != nil {
		if detail := strings.TrimSpace(errb.String()); runErr != nil && detail != "" {
			return home.Verdict{}, fmt.Errorf("running %s -test-config: %v: %s", binPath, runErr, detail)
		} else if runErr != nil {
			return home.Verdict{}, fmt.Errorf("running %s -test-config: %w", binPath, runErr)
		}
		return home.Verdict{}, fmt.Errorf("%s -test-config produced no parseable verdict", binPath)
	}
	return v, nil
}

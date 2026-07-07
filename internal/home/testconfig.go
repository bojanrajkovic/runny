package home

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

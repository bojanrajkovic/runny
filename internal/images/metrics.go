package images

import (
	"time"

	"github.com/bojanrajkovic/runny/internal/obs"
)

// Metrics is the ensurer-scope metrics seam: per-underlying-work truths the
// cycle event stream can't carry, because a shared pull belongs to no single
// cycle. Injected the way progress reporting is — the daemon wires it, tests
// fake it, and nil (the whole struct or a field) disables recording. No
// telemetry types cross this seam.
type Metrics struct {
	// PullDone fires once per underlying image pull, at the shared puller's
	// terminal outcome — never once per subscriber. d is the puller's
	// lifetime (including disk holds and re-attempts: the operator-felt
	// cost); bytes is the total transferred across attempts, which can
	// exceed the image size when a failed attempt re-transferred layers. A
	// puller cancelled before a terminal outcome (its last subscriber left)
	// records nothing.
	PullDone func(outcome string, d time.Duration, bytes int64)
	// TarballDownloadDone fires once per actual runner-tarball download —
	// never for a cache hit or a slot that waited out a peer's download.
	TarballDownloadDone func(outcome string, d time.Duration)
}

func (m *Metrics) pullDone(outcome string, d time.Duration, bytes int64) {
	if m == nil || m.PullDone == nil {
		return
	}
	m.PullDone(outcome, d, bytes)
}

func (m *Metrics) tarballDownloadDone(outcome string, d time.Duration) {
	if m == nil || m.TarballDownloadDone == nil {
		return
	}
	m.TarballDownloadDone(outcome, d)
}

// outcomeOf maps an error to the closed ok/error outcome vocabulary shared
// with obs.
func outcomeOf(err error) string {
	if err != nil {
		return string(obs.OutcomeError)
	}
	return string(obs.OutcomeOK)
}

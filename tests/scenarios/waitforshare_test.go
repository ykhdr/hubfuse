package scenarios_test

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// capturingT records what an assertion would have reported, so a test can make
// claims about a FAILURE message without failing itself.
type capturingT struct{ msgs []string }

func (c *capturingT) Errorf(format string, args ...interface{}) {
	c.msgs = append(c.msgs, fmt.Sprintf(format, args...))
}

// TestWaitForShare_ReportsTheLastObservationOnTimeout pins the assumption
// waitForShare's diagnostic rests on: that testify formats an Eventuallyf
// message's arguments when the assertion FAILS, not when it is called.
//
// The distinction is the entire value of the message. waitForShare's
// observation (the last SFTP listing, and any error from it) is empty at call
// time and only filled in by the polling closure. If testify ever formatted
// eagerly, the message would keep printing "(empty)" and "none" forever — and
// that is worse than printing nothing, because it reads as evidence that the
// listing really was empty. A maintainer would chase a broken config reload
// when the truth was a slow runner.
//
// This is the failure issue #85 was: a ten-second budget expiring on a loaded
// CI runner, reported only as "Condition never satisfied", which cost a rerun
// to tell apart from a real regression.
func TestWaitForShare_ReportsTheLastObservationOnTimeout(t *testing.T) {
	t.Parallel()

	var (
		mu      sync.Mutex
		seen    []string
		lastErr error
	)

	rec := &capturingT{}
	assert.Eventuallyf(rec, func() bool {
		// Stand in for the real polling body: it observes, records, and reports
		// that the alias is still missing.
		mu.Lock()
		seen = []string{"other-share", "another-share"}
		lastErr = errors.New("sftp: permission denied")
		mu.Unlock()
		return false
	}, 300*time.Millisecond, 50*time.Millisecond,
		"share %q never appeared; last listing: %v, last error: %v",
		"wanted-alias",
		&deferredStrings{mu: &mu, v: &seen},
		&deferredError{mu: &mu, v: &lastErr})

	joined := strings.Join(rec.msgs, "\n")

	assert.Contains(t, joined, "other-share, another-share",
		"the message must carry what was actually listed, which is only known after polling started")
	assert.Contains(t, joined, "sftp: permission denied",
		"and the error that came with it, which distinguishes a dead session from a slow reload")
	assert.NotContains(t, joined, "(empty)",
		"an empty observation here would mean testify formatted eagerly and the diagnostic is useless")
}

// TestDeferredObservers_RenderPlaceholdersWhenNothingWasSeen covers the other
// half: when polling genuinely never saw anything, the message must say so in
// words rather than printing a bare "[]" and "<nil>" that read like noise.
func TestDeferredObservers_RenderPlaceholdersWhenNothingWasSeen(t *testing.T) {
	t.Parallel()

	var (
		mu      sync.Mutex
		seen    []string
		lastErr error
	)

	assert.Equal(t, "(empty)", (&deferredStrings{mu: &mu, v: &seen}).String())
	assert.Equal(t, "none", (&deferredError{mu: &mu, v: &lastErr}).String())
}

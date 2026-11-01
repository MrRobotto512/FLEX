//go:build !windows

package server

import (
	"testing"
	"time"
)

func setWatchPollForTest(t *testing.T, d time.Duration) {
	old := watchPoll
	watchPoll = d
	t.Cleanup(func() { watchPoll = old })
}

package server

import (
	"testing"
	"time"
)

// Windows watching is by notification; there is nothing to poll.
func setWatchPollForTest(t *testing.T, d time.Duration) {}

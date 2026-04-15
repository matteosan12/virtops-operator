package safety

// Package safety provides retry, backoff, and circuit-breaker primitives to reduce impact
// in case of repeated errors during rotations.

// Placeholder for build.

type Options struct {
	RetryAttempts  int
	BackoffSeconds int
	PauseOnError   bool
	MaxFailures    int
}

func ShouldPause(o Options, failures int) bool { return false }

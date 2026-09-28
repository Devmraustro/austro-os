package execution

import "time"

// RetryDelay returns the pause before the flight after a failed attempt,
// exponential with a hard cap: base*2^(attempt-1). attempt is the failed
// attempt number (1-based); the sequence is base, 2*base, 4*base, ... and the
// cap bounds tail latencies however deep the retry budget goes.
func RetryDelay(attempt int, base, cap time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := base
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= cap {
			return cap
		}
	}
	if d >= cap {
		return cap
	}
	return d
}

// AttemptWindow is the lease budget granted to attempt n of maxAttempts: the
// full budget for the first attempt, shrinking linearly so late attempts run
// under tighter deadlines without starving the worker pool. A single max_attempts
// grants the full window; the sequence is 1, (max-1)/max, ... , 1/max of full.
func AttemptWindow(full time.Duration, attempt, maxAttempts int) time.Duration {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	if attempt < 1 {
		attempt = 1
	}
	if attempt > maxAttempts {
		attempt = maxAttempts
	}
	if maxAttempts == 1 {
		return full
	}
	window := full * time.Duration(maxAttempts-attempt+1) / time.Duration(maxAttempts)
	if window < time.Second {
		return time.Second
	}
	return window
}

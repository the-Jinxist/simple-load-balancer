// Package pkg contains shared helpers for the load balancer.
package pkg

import (
	"math/rand/v2"
	"time"
)

// Jitter returns base adjusted uniformly within +/- fraction.
//
// fraction is clamped to [0, 1]. A non-positive base or fraction disables
// jitter.
func Jitter(base time.Duration, fraction float64) time.Duration {
	if base <= 0 {
		return 0
	}
	if fraction <= 0 {
		return base
	}
	if fraction > 1 {
		fraction = 1
	}

	factor := 1 + fraction*(2*rand.Float64()-1) // [1-fraction, 1+fraction)
	return time.Duration(float64(base) * factor)
}

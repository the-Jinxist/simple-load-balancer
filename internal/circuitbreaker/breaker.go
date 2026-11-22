// Package circuitbreaker contains a generic circuit breaker scaffold.
//
// It is not wired into the load balancer yet; see IMPROVEMENTS.md (P2-17).
package circuitbreaker

import (
	"context"
	"time"
)

// BreakerState identifies a circuit breaker state.
//
// The values below are placeholders and currently all have the zero value; they
// need distinct assignments before use.
type BreakerState string

var (
	// Open, Closed, and HalfOpened are the intended breaker states.
	Open       BreakerState
	Closed     BreakerState
	HalfOpened BreakerState
)

// BreakerCtxExec is an operation guarded by a CircuitBreaker.
type BreakerCtxExec[I any, O any] func(ctx context.Context, value I) (result O, err error)

// CircuitBreaker is a generic circuit breaker scaffold.
//
// The implementation is incomplete: state is not synchronized and there is no
// public execution method yet. See IMPROVEMENTS.md (P2-17).
type CircuitBreaker[I any, O any] struct {
	state                BreakerState
	delayUntilHalfOpened time.Duration
	exec                 BreakerCtxExec[I, O]
}

// startHalfOpenCountdown marks the breaker as half-open after
// delayUntilHalfOpened elapses.
func (c *CircuitBreaker[I, O]) startHalfOpenCountdown() {
	<-time.Tick(c.delayUntilHalfOpened)
	c.state = HalfOpened
}

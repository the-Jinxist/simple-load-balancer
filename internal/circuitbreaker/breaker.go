package circuitbreaker

import (
	"context"
	"time"
)

type BreakerState string

var (
	Open       BreakerState
	Closed     BreakerState
	HalfOpened BreakerState
)

type BreakerCtxExec[I any, O any] func(ctx context.Context, value I) (result O, err error)

type CircuitBreaker[I any, O any] struct {
	state                BreakerState
	delayUntilHalfOpened time.Duration
	exec                 BreakerCtxExec[I, O]
}

func (c *CircuitBreaker[I, O]) startHalfOpenCountdown() {
	<-time.Tick(c.delayUntilHalfOpened)
	c.state = HalfOpened
}

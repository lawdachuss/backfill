package internal

import (
	"sync"
	"sync/atomic"
	"time"
)

// State represents the circuit breaker state.
type State int32

const (
	StateClosed   State = iota // Normal operation
	StateOpen                  // Failing — reject requests
	StateHalfOpen              // Testing if recovered
)

// CircuitBreaker protects Chaturbate API calls from cascading failure.
// When the error rate exceeds the threshold, it breaks the circuit and
// rejects all requests for a cooldown period, giving the upstream time
// to recover.
type CircuitBreaker struct {
	state           atomic.Int32
	failures        atomic.Int64
	successes       atomic.Int64
	lastStateChange atomic.Int64 // unix nanos

	threshold     float64       // error ratio that triggers open (e.g. 0.2 = 20%)
	minSamples    int64         // minimum requests before evaluating
	cooldown      time.Duration // time to stay open before half-open
	halfOpenMax   int64         // max half-open probes before re-evaluating
	halfOpenCount atomic.Int64

	mu sync.Mutex
}

// BreakerConfig configures the circuit breaker.
type BreakerConfig struct {
	Threshold   float64       // error ratio threshold (default 0.2)
	MinSamples  int64         // min requests before evaluating (default 10)
	Cooldown    time.Duration // time to stay open (default 10s)
	HalfOpenMax int64         // max half-open probes (default 3)
}

// DefaultBreakerConfig returns sensible defaults for Chaturbate API calls.
func DefaultBreakerConfig() BreakerConfig {
	return BreakerConfig{
		Threshold:   0.2,              // open at 20% error rate
		MinSamples:  10,               // need 10 samples first
		Cooldown:    10 * time.Second, // stay open for 10s
		HalfOpenMax: 3,                // try 3 probes in half-open
	}
}

// NewCircuitBreaker creates a circuit breaker with the given config.
func NewCircuitBreaker(cfg BreakerConfig) *CircuitBreaker {
	return &CircuitBreaker{
		threshold:   cfg.Threshold,
		minSamples:  cfg.MinSamples,
		cooldown:    cfg.Cooldown,
		halfOpenMax: cfg.HalfOpenMax,
	}
}

// Allow returns true if the request should proceed.
func (cb *CircuitBreaker) Allow() bool {
	state := State(cb.state.Load())
	switch state {
	case StateClosed:
		return true
	case StateOpen:
		// Check if cooldown elapsed → transition to half-open
		changed := cb.lastStateChange.Load()
		if time.Since(time.Unix(0, changed)) >= cb.cooldown {
			if cb.state.CompareAndSwap(int32(StateOpen), int32(StateHalfOpen)) {
				cb.halfOpenCount.Store(0)
				return true
			}
		}
		return false
	case StateHalfOpen:
		// Allow up to halfOpenMax probes
		count := cb.halfOpenCount.Add(1)
		if count <= cb.halfOpenMax {
			return true
		}
		return false
	default:
		return true
	}
}

// Success records a successful request.
func (cb *CircuitBreaker) Success() {
	state := State(cb.state.Load())
	if state == StateHalfOpen {
		// Single success in half-open → close the circuit
		cb.state.Store(int32(StateClosed))
		cb.halfOpenCount.Store(0)
		cb.resetCounters()
		return
	}
	cb.successes.Add(1)
	cb.evaluate()
}

// Failure records a failed request.
func (cb *CircuitBreaker) Failure() {
	state := State(cb.state.Load())
	if state == StateHalfOpen {
		// Failure in half-open → back to open
		cb.state.Store(int32(StateOpen))
		cb.lastStateChange.Store(time.Now().UnixNano())
		cb.halfOpenCount.Store(0)
		return
	}
	cb.failures.Add(1)
	cb.evaluate()
}

// evaluate checks if the error rate exceeds the threshold and opens if so.
func (cb *CircuitBreaker) evaluate() {
	fail := cb.failures.Load()
	succ := cb.successes.Load()
	total := fail + succ

	if total < cb.minSamples {
		return
	}

	rate := float64(fail) / float64(total)
	if rate >= cb.threshold {
		if cb.state.CompareAndSwap(int32(StateClosed), int32(StateOpen)) {
			cb.lastStateChange.Store(time.Now().UnixNano())
		}
	}
}

func (cb *CircuitBreaker) resetCounters() {
	cb.failures.Store(0)
	cb.successes.Store(0)
}

// chaturbateBreaker is the global circuit breaker for Chaturbate API calls.
var chaturbateBreaker = NewCircuitBreaker(DefaultBreakerConfig())

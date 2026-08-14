package orchestrator

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jawalab-com/payrouter/internal/gateway"
)

// CircuitState represents the current state of a gateway circuit breaker.
type CircuitState string

const (
	StateClosed   CircuitState = "closed"    // Healthy, traffic allowed
	StateOpen     CircuitState = "open"      // Unhealthy, traffic diverted
	StateHalfOpen CircuitState = "half_open" // Testing recovery with canary probes
)

// BreakerConfig configures the in-memory circuit breaker.
type BreakerConfig struct {
	FailureThreshold int           // Consecutive failures before tripping to Open (default: 3)
	CooldownDuration time.Duration // Time to stay in Open before probing HalfOpen (default: 30s)
}

// DefaultBreakerConfig returns sensible in-memory defaults without extra infra.
func DefaultBreakerConfig() BreakerConfig {
	return BreakerConfig{
		FailureThreshold: 3,
		CooldownDuration: 30 * time.Second,
	}
}

// gatewayState tracks the health state of a single gateway.
type gatewayState struct {
	state               CircuitState
	consecutiveFailures int
	trippedAt           time.Time
	halfOpenProbing     bool
}

// CircuitBreaker manages in-memory, zero-dependency health tracking and failover
// for all candidate payment gateways.
type CircuitBreaker struct {
	mu     sync.RWMutex
	cfg    BreakerConfig
	states map[string]*gatewayState
	nowFn  func() time.Time // time mock for deterministic unit testing
}

// NewCircuitBreaker creates a new in-memory CircuitBreaker.
func NewCircuitBreaker(cfg BreakerConfig) *CircuitBreaker {
	if cfg.FailureThreshold <= 0 {
		cfg.FailureThreshold = 3
	}
	if cfg.CooldownDuration <= 0 {
		cfg.CooldownDuration = 30 * time.Second
	}
	return &CircuitBreaker{
		cfg:    cfg,
		states: make(map[string]*gatewayState),
		nowFn:  time.Now,
	}
}

func (cb *CircuitBreaker) getOrCreate(name string) *gatewayState {
	st, ok := cb.states[name]
	if !ok {
		st = &gatewayState{state: StateClosed}
		cb.states[name] = st
	}
	return st
}

// Allow reports whether a gateway is eligible to receive traffic.
// If the circuit is Open and the cooldown duration has passed, it transitions
// to HalfOpen to allow a recovery probe.
func (cb *CircuitBreaker) Allow(gatewayName string) bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	st := cb.getOrCreate(gatewayName)
	now := cb.nowFn()

	switch st.state {
	case StateClosed:
		return true
	case StateOpen:
		if now.Sub(st.trippedAt) >= cb.cfg.CooldownDuration {
			st.state = StateHalfOpen
			st.halfOpenProbing = true
			return true
		}
		return false
	case StateHalfOpen:
		// In HalfOpen, allow traffic if not already actively probing
		if !st.halfOpenProbing {
			st.halfOpenProbing = true
			return true
		}
		return false
	default:
		return true
	}
}

// State returns the current circuit state of the named gateway.
func (cb *CircuitBreaker) State(gatewayName string) CircuitState {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	st := cb.getOrCreate(gatewayName)
	if st.state == StateOpen && cb.nowFn().Sub(st.trippedAt) >= cb.cfg.CooldownDuration {
		st.state = StateHalfOpen
		st.halfOpenProbing = false
	}
	return st.state
}

// RecordSuccess records a successful gateway operation, resetting failures and
// closing the circuit.
func (cb *CircuitBreaker) RecordSuccess(gatewayName string) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	st := cb.getOrCreate(gatewayName)
	st.state = StateClosed
	st.consecutiveFailures = 0
	st.halfOpenProbing = false
}

// RecordFailure records a failed gateway operation. If consecutive failures reach
// the configured threshold, the circuit trips to Open.
func (cb *CircuitBreaker) RecordFailure(gatewayName string, err error) {
	if !IsUpstreamFailure(err) {
		return // Client input errors, unsupported instrument types, etc. do not trip the breaker
	}

	cb.mu.Lock()
	defer cb.mu.Unlock()

	st := cb.getOrCreate(gatewayName)
	st.consecutiveFailures++
	st.halfOpenProbing = false

	if st.consecutiveFailures >= cb.cfg.FailureThreshold || st.state == StateHalfOpen {
		st.state = StateOpen
		st.trippedAt = cb.nowFn()
	}
}

// Reset clears all failure counters and resets all circuits to Closed.
func (cb *CircuitBreaker) Reset(gatewayName string) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	st := cb.getOrCreate(gatewayName)
	st.state = StateClosed
	st.consecutiveFailures = 0
	st.halfOpenProbing = false
}

// IsUpstreamFailure determines whether an error is caused by gateway server degradation
// (5xx, network timeout, connection refused, context cancellation) rather than client
// validation or input error.
func IsUpstreamFailure(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gateway.ErrInstrumentUnsupported) {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	// Any other gateway API response errors (status 500+, network failure)
	return true
}

// Package httpretry provides bounded, context-aware retries for HTTP
// operations that are safe to repeat.
package httpretry

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
)

const retryDrainLimit = 64 << 10

// RequestFactory creates a fresh request for each attempt. A fresh body is
// essential because HTTP request bodies are consumed during transmission.
type RequestFactory func() (*http.Request, error)

// Policy bounds attempts and delays. Sleep and Jitter are injectable so tests
// can remain fast and deterministic.
type Policy struct {
	MaxAttempts   int
	BaseDelay     time.Duration
	MaxDelay      time.Duration
	MaxRetryAfter time.Duration
	Sleep         func(context.Context, time.Duration) error
	Jitter        func() float64
}

// DefaultPolicy is deliberately conservative: one request plus two retries,
// with a maximum server-directed delay of thirty seconds.
func DefaultPolicy() Policy {
	return Policy{
		MaxAttempts:   3,
		BaseDelay:     500 * time.Millisecond,
		MaxDelay:      8 * time.Second,
		MaxRetryAfter: 30 * time.Second,
		Sleep:         sleepContext,
		Jitter:        rand.Float64,
	}
}

// Do sends an HTTP request and retries only when safe is true. Callers must
// mark state-changing requests unsafe unless they have an idempotency key or
// another protocol-level guarantee against duplicate writes.
func Do(ctx context.Context, client *http.Client, makeRequest RequestFactory, safe bool, logger *zap.Logger, operation string, policy Policy) (*http.Response, error) {
	if client == nil || makeRequest == nil {
		return nil, fmt.Errorf("HTTP retry requires a client and request factory")
	}
	policy = normalize(policy)
	if logger == nil {
		logger = zap.NewNop()
	}

	for attempt := 1; ; attempt++ {
		request, err := makeRequest()
		if err != nil {
			return nil, fmt.Errorf("create %s request: %w", operation, err)
		}
		response, err := client.Do(request)
		if err == nil && (!safe || !retryable(response)) {
			return response, nil
		}
		if !safe || attempt == policy.MaxAttempts || ctx.Err() != nil {
			if err != nil {
				if response != nil {
					drainAndClose(response.Body)
				}
				return nil, err
			}
			return response, nil
		}

		if response != nil {
			drainAndClose(response.Body)
		}
		delay := backoff(policy, attempt, response)
		fields := []zap.Field{
			zap.String("operation", operation),
			zap.Int("attempt", attempt),
			zap.Int("max_attempts", policy.MaxAttempts),
			zap.Duration("retry_in", delay),
		}
		if response != nil {
			fields = append(fields, zap.Int("http_status", response.StatusCode))
		}
		if err != nil {
			fields = append(fields, zap.Error(err))
		}
		logger.Warn("temporary HTTP failure; retrying", fields...)
		if err := policy.Sleep(ctx, delay); err != nil {
			return nil, err
		}
	}
}

func normalize(policy Policy) Policy {
	defaults := DefaultPolicy()
	if policy.MaxAttempts < 1 {
		policy.MaxAttempts = defaults.MaxAttempts
	}
	if policy.BaseDelay < 0 {
		policy.BaseDelay = 0
	}
	if policy.BaseDelay == 0 && policy.Sleep == nil {
		policy.BaseDelay = defaults.BaseDelay
	}
	if policy.MaxDelay <= 0 {
		policy.MaxDelay = defaults.MaxDelay
	}
	if policy.MaxRetryAfter <= 0 {
		policy.MaxRetryAfter = defaults.MaxRetryAfter
	}
	if policy.Sleep == nil {
		policy.Sleep = defaults.Sleep
	}
	if policy.Jitter == nil {
		policy.Jitter = defaults.Jitter
	}
	return policy
}

func retryable(response *http.Response) bool {
	if response == nil {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(response.Header.Get("X-Should-Retry"))) {
	case "false":
		return false
	case "true":
		return true
	}
	switch response.StatusCode {
	case http.StatusRequestTimeout, http.StatusTooEarly,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	case http.StatusTooManyRequests:
		// A Retry-After header distinguishes temporary throttling from many
		// permanent quota/billing failures that also use HTTP 429.
		return strings.TrimSpace(response.Header.Get("Retry-After")) != ""
	default:
		return false
	}
}

func backoff(policy Policy, attempt int, response *http.Response) time.Duration {
	if response != nil {
		if delay, ok := parseRetryAfter(response.Header.Get("Retry-After"), time.Now()); ok {
			if delay > policy.MaxRetryAfter {
				return policy.MaxRetryAfter
			}
			return delay
		}
	}
	delay := policy.BaseDelay
	for i := 1; i < attempt && delay < policy.MaxDelay; i++ {
		if delay > policy.MaxDelay/2 {
			delay = policy.MaxDelay
			break
		}
		delay *= 2
	}
	if delay > policy.MaxDelay {
		delay = policy.MaxDelay
	}
	factor := policy.Jitter()
	if factor < 0 {
		factor = 0
	}
	if factor > 1 {
		factor = 1
	}
	return time.Duration(float64(delay) * factor)
}

func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second, true
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	delay := when.Sub(now)
	if delay < 0 {
		delay = 0
	}
	return delay, true
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func drainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, retryDrainLimit))
	_ = body.Close()
}

package httpretry

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

func immediatePolicy() Policy {
	return Policy{
		MaxAttempts: 3, MaxDelay: time.Millisecond, MaxRetryAfter: time.Millisecond,
		Sleep:  func(context.Context, time.Duration) error { return nil },
		Jitter: func() float64 { return 0 },
	}
}

func TestDoRetriesTemporarySafeRequest(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		if attempts < 3 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, "busy", http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	makeRequest := func() (*http.Request, error) {
		return http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	}
	response, err := Do(context.Background(), server.Client(), makeRequest, true, zap.NewNop(), "test", immediatePolicy())
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || attempts != 3 {
		t.Fatalf("status=%d attempts=%d", response.StatusCode, attempts)
	}
}

func TestDoDoesNotRetryUnsafeOrPermanentFailures(t *testing.T) {
	for _, test := range []struct {
		name   string
		safe   bool
		status int
		header string
	}{
		{"unsafe", false, http.StatusServiceUnavailable, ""},
		{"bad request", true, http.StatusBadRequest, ""},
		{"permanent quota", true, http.StatusTooManyRequests, ""},
		{"explicit refusal", true, http.StatusServiceUnavailable, "false"},
	} {
		t.Run(test.name, func(t *testing.T) {
			attempts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				attempts++
				if test.header != "" {
					w.Header().Set("X-Should-Retry", test.header)
				}
				w.WriteHeader(test.status)
			}))
			defer server.Close()
			response, err := Do(context.Background(), server.Client(), func() (*http.Request, error) {
				return http.NewRequest(http.MethodGet, server.URL, nil)
			}, test.safe, nil, "test", immediatePolicy())
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if attempts != 1 {
				t.Fatalf("attempts=%d", attempts)
			}
		})
	}
}

func TestDoReturnsFinalTemporaryResponse(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	response, err := Do(context.Background(), server.Client(), func() (*http.Request, error) {
		return http.NewRequest(http.MethodGet, server.URL, nil)
	}, true, nil, "temporary", immediatePolicy())
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable || attempts != 3 {
		t.Fatalf("status=%d attempts=%d", response.StatusCode, attempts)
	}
}

type failingTransport struct{ attempts int }

func (f *failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	f.attempts++
	return nil, errors.New("temporary transport failure")
}

func TestDoTransportAndValidationErrors(t *testing.T) {
	transport := &failingTransport{}
	client := &http.Client{Transport: transport}
	_, err := Do(context.Background(), client, func() (*http.Request, error) {
		return http.NewRequest(http.MethodGet, "http://example.test", nil)
	}, true, nil, "transport", immediatePolicy())
	if err == nil || transport.attempts != 3 {
		t.Fatalf("error=%v attempts=%d", err, transport.attempts)
	}
	if _, err = Do(context.Background(), nil, nil, true, nil, "nil", Policy{}); err == nil {
		t.Fatal("nil dependencies should fail")
	}
	_, err = Do(context.Background(), http.DefaultClient, func() (*http.Request, error) {
		return nil, errors.New("factory")
	}, true, nil, "factory", Policy{})
	if err == nil || !strings.Contains(err.Error(), "factory") {
		t.Fatal(err)
	}
}

func TestDoHonorsContextDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("network")
	})}
	policy := immediatePolicy()
	policy.Sleep = func(context.Context, time.Duration) error {
		cancel()
		return context.Canceled
	}
	_, err := Do(ctx, client, func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, "http://example.test", nil)
	}, true, nil, "cancel", policy)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestRetryHelpers(t *testing.T) {
	now := time.Now()
	if _, ok := parseRetryAfter("", now); ok {
		t.Fatal("empty Retry-After accepted")
	}
	if delay, ok := parseRetryAfter("2", now); !ok || delay != 2*time.Second {
		t.Fatalf("delay=%v ok=%v", delay, ok)
	}
	if _, ok := parseRetryAfter("invalid", now); ok {
		t.Fatal("invalid Retry-After accepted")
	}
	if delay, ok := parseRetryAfter(now.Add(time.Second).UTC().Format(http.TimeFormat), now); !ok || delay < 0 || delay > time.Second {
		t.Fatalf("date delay=%v ok=%v", delay, ok)
	}
	if delay, ok := parseRetryAfter(now.Add(-time.Second).UTC().Format(http.TimeFormat), now); !ok || delay != 0 {
		t.Fatalf("past date delay=%v ok=%v", delay, ok)
	}
	policy := immediatePolicy()
	policy.BaseDelay = 2 * time.Second
	policy.MaxDelay = 3 * time.Second
	policy.Jitter = func() float64 { return 2 }
	if got := backoff(policy, 3, nil); got != 3*time.Second {
		t.Fatal(got)
	}
	policy.Jitter = func() float64 { return -1 }
	if got := backoff(policy, 1, nil); got != 0 {
		t.Fatal(got)
	}
	policy.BaseDelay = 4 * time.Second
	policy.MaxDelay = 3 * time.Second
	policy.Jitter = func() float64 { return 1 }
	if got := backoff(policy, 1, nil); got != 3*time.Second {
		t.Fatal(got)
	}
	response := &http.Response{Header: http.Header{"Retry-After": []string{"30"}}}
	policy.MaxRetryAfter = 2 * time.Second
	if got := backoff(policy, 1, response); got != 2*time.Second {
		t.Fatal(got)
	}
	if !retryable(nil) {
		t.Fatal("nil response should represent a retryable transport failure")
	}
	for _, status := range []int{http.StatusRequestTimeout, http.StatusTooEarly, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		if !retryable(&http.Response{StatusCode: status, Header: make(http.Header)}) {
			t.Fatalf("status %d should be retryable", status)
		}
	}
	if !retryable(&http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{"X-Should-Retry": []string{"true"}}}) {
		t.Fatal("explicit retry request ignored")
	}
	negative := normalize(Policy{BaseDelay: -time.Second, Sleep: func(context.Context, time.Duration) error { return nil }})
	if negative.BaseDelay != 0 {
		t.Fatal(negative.BaseDelay)
	}
	if err := sleepContext(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepContext(cancelled, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	drainAndClose(nil)
	body := io.NopCloser(strings.NewReader("discard me"))
	drainAndClose(body)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDingtalkTokenRefreshCoalescesAndWaitersCancelIndependently(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var requests atomic.Int32
	a, _ := dingtalkTestAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			close(started)
		}
		<-release
		dingtalkTestToken(w)
	})
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, err := a.accessToken(ctx); first <- err }()
	<-started
	const count = 20
	var wg sync.WaitGroup
	wg.Add(count)
	for range count {
		go func() {
			defer wg.Done()
			token, err := a.accessToken(context.Background())
			if err != nil || token != "test-token" {
				t.Errorf("token = %q, error = %v", token, err)
			}
		}()
	}
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("first wait error = %v", err)
	}
	close(release)
	wg.Wait()
	if requests.Load() != 1 {
		t.Fatalf("refresh requests = %d, want 1", requests.Load())
	}
	// Concurrent expiry must produce one additional refresh, rather than one
	// request per sending goroutine.
	a.tokens.mu.Lock()
	a.tokens.expires = time.Now().Add(-time.Second)
	a.tokens.mu.Unlock()
	wg.Add(count)
	for range count {
		go func() { defer wg.Done(); _, _ = a.accessToken(context.Background()) }()
	}
	wg.Wait()
	if requests.Load() != 2 {
		t.Fatalf("total refresh requests = %d, want 2", requests.Load())
	}
}

func TestDingtalkTokenExpiryAndInvalidResponses(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		category   string
	}{
		{"valid", `{"accessToken":"test-token","expireIn":7200}`, 200, ""},
		{"short-validity", `{"accessToken":"test-token","expireIn":1}`, 200, ""},
		{"zero-validity", `{"accessToken":"test-token","expireIn":0}`, 200, "auth_failed"},
		{"negative-validity", `{"accessToken":"test-token","expireIn":-1}`, 200, "auth_failed"},
		{"overflow-validity", `{"accessToken":"test-token","expireIn":9223372036854775807}`, 200, "auth_failed"},
		{"missing-token", `{"expireIn":7200}`, 200, "auth_failed"},
		{"explicit-error", `{"code":"InvalidClient","accessToken":"test-token","expireIn":7200}`, 200, "auth_failed"},
		{"unauthorized", `{}`, 401, "auth_failed"},
		{"forbidden", `{}`, 403, "auth_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := dingtalkTestAPI(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, tc.body)
			})
			before := time.Now()
			token, err := a.accessToken(context.Background())
			if tc.category != "" {
				dingtalkTestCategory(t, err, tc.category)
				if token != "" {
					t.Fatal("invalid response cached a token")
				}
				return
			}
			if err != nil || token != "test-token" {
				t.Fatalf("token = %q, error = %v", token, err)
			}
			a.tokens.mu.Lock()
			expires := a.tokens.expires
			a.tokens.mu.Unlock()
			remaining := expires.Sub(before)
			if tc.name == "valid" && (remaining < 7139*time.Second || remaining > 7141*time.Second) {
				t.Fatalf("early expiry = %v, want approximately 7140 seconds", remaining)
			}
			if tc.name == "short-validity" && (remaining < 890*time.Millisecond || remaining > time.Second) {
				t.Fatalf("short early expiry = %v, want approximately 900 milliseconds", remaining)
			}
		})
	}
}

func TestDingtalkTokenLifecycleStopsRefreshAndCloseJoins(t *testing.T) {
	started := make(chan struct{})
	exited := make(chan struct{})
	a, cancel := dingtalkTestAPI(t, func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
		close(exited)
	})
	result := make(chan error, 1)
	go func() { _, err := a.accessToken(context.Background()); result <- err }()
	<-started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("refresh waiter error = %v", err)
	}
	a.Close()
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("refresh HTTP request did not exit")
	}
	a.tokens.mu.Lock()
	active := a.tokens.refresh
	a.tokens.mu.Unlock()
	if active != nil {
		t.Fatal("Close returned before refresh finished")
	}
}

func TestDingtalkTokenOldFailureDoesNotInvalidateFreshToken(t *testing.T) {
	a, _ := dingtalkTestAPI(t, func(w http.ResponseWriter, _ *http.Request) { dingtalkTestToken(w) })
	if _, err := a.accessToken(context.Background()); err != nil {
		t.Fatal(err)
	}
	a.invalidateToken("older-token")
	a.tokens.mu.Lock()
	defer a.tokens.mu.Unlock()
	if a.tokens.token != "test-token" {
		t.Fatal("older unauthorized response cleared fresh token")
	}
}

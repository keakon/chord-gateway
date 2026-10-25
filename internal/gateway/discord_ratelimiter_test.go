package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/disgoorg/disgo/rest"

	discord "github.com/keakon/chord-gateway/internal/platform/discord"
)

func discordRateLimiterEndpoint(method, major string) *rest.CompiledEndpoint {
	return &rest.CompiledEndpoint{Endpoint: rest.NewEndpoint(method, "/channels/{channel.id}/messages"), MajorParams: major}
}

func discordRateLimiterResponse(status int, headers map[string]string) *http.Response {
	response := &http.Response{StatusCode: status, Header: make(http.Header)}
	for key, value := range headers {
		response.Header.Set(key, value)
	}
	return response
}

func TestDiscordRateLimiterRouteSerializationAndErrorUnlock(t *testing.T) {
	l := discord.NewDiscordRateLimiter(t.Context())
	defer l.Close(t.Context())
	endpoint := discordRateLimiterEndpoint(http.MethodPost, "channel-a")
	if err := l.Wait(t.Context(), endpoint); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := l.Wait(ctx, endpoint); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("same route concurrently acquired: %v", err)
	}
	for _, other := range []*rest.CompiledEndpoint{
		discordRateLimiterEndpoint(http.MethodGet, "channel-a"),
		discordRateLimiterEndpoint(http.MethodPost, "channel-b"),
	} {
		if err := l.Wait(t.Context(), other); err != nil {
			t.Fatal("independent route could not acquire", err)
		}
		if err := l.Unlock(other, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Unlock(endpoint, nil); err != nil {
		t.Fatal(err)
	}
	if err := l.Wait(t.Context(), endpoint); err != nil {
		t.Fatal("nil response retained route lock", err)
	}
	if err := l.Unlock(endpoint, discordRateLimiterResponse(http.StatusTooManyRequests, nil)); !errors.Is(err, discord.ErrDiscordRateLimitHeaders) {
		t.Fatal("missing retry delay did not stop SDK retry", err)
	}
	if err := l.Wait(t.Context(), endpoint); err != nil {
		t.Fatal("invalid response retained route lock", err)
	}
	if err := l.Unlock(endpoint, nil); err != nil {
		t.Fatal(err)
	}
}

func TestDiscordRateLimiterGlobalHeaderOnlyOnExplicitTrue(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header map[string]string
		global bool
	}{
		{"true", map[string]string{"X-RateLimit-Global": "true"}, true},
		{"upper", map[string]string{"X-RateLimit-Global": "TRUE"}, true},
		{"false", map[string]string{"X-RateLimit-Global": "false"}, false},
		{"empty", map[string]string{"X-RateLimit-Global": ""}, false},
		{"scope_global", map[string]string{"X-RateLimit-Scope": "global"}, true},
		{"scope_user", map[string]string{"X-RateLimit-Scope": "user"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := discord.NewDiscordRateLimiter(t.Context())
			defer l.Close(t.Context())
			route := discordRateLimiterEndpoint(http.MethodPost, "route")
			if err := l.Wait(t.Context(), route); err != nil {
				t.Fatal(err)
			}
			header := map[string]string{"Retry-After": "0.05"}
			for key, value := range tc.header {
				header[key] = value
			}
			if err := l.Unlock(route, discordRateLimiterResponse(http.StatusTooManyRequests, header)); err != nil {
				t.Fatal(err)
			}
			global := !l.GlobalResetForTest().IsZero()
			reset, remaining, _, _ := l.BucketForTest(route)
			if global != tc.global {
				t.Fatalf("global limit = %t want %t", global, tc.global)
			}
			// A global limit blocks every route, so the bucket itself keeps its
			// remaining count and its own reset is never written.
			if reset.IsZero() != tc.global || (remaining != 0) != tc.global {
				t.Fatal("global limit and route reset disagree", reset, remaining)
			}
		})
	}
}

func TestDiscordRateLimiterFractionalRouteAndGlobalReset(t *testing.T) {
	l := discord.NewDiscordRateLimiter(t.Context())
	defer l.Close(t.Context())
	route := discordRateLimiterEndpoint(http.MethodPost, "route")
	global := discordRateLimiterEndpoint(http.MethodGet, "global")
	if err := l.Wait(t.Context(), route); err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	if err := l.Unlock(route, discordRateLimiterResponse(http.StatusOK, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset-After": "0.04"})); err != nil {
		t.Fatal(err)
	}
	routeReset, _, _, _ := l.BucketForTest(route)
	if delta := routeReset.Sub(before); delta < 40*time.Millisecond || delta > time.Second {
		t.Fatal("fractional reset was truncated", delta)
	}
	if err := l.Wait(t.Context(), global); err != nil {
		t.Fatal(err)
	}
	if err := l.Unlock(global, discordRateLimiterResponse(http.StatusTooManyRequests, map[string]string{"Retry-After": "0.08", "X-RateLimit-Global": "true"})); err != nil {
		t.Fatal(err)
	}
	globalReset := l.GlobalResetForTest()
	if !globalReset.After(routeReset) {
		t.Fatal("test global delay did not exceed route delay")
	}
	if err := l.Wait(t.Context(), route); err != nil {
		t.Fatal(err)
	}
	if time.Now().Before(globalReset) {
		t.Fatal("route reset bypassed longer global limit")
	}
	if err := l.Unlock(route, nil); err != nil {
		t.Fatal(err)
	}
}

func TestDiscordRateLimiterLongerRouteResetAndConcurrentGlobalUpdates(t *testing.T) {
	l := discord.NewDiscordRateLimiter(t.Context())
	defer l.Close(t.Context())
	route := discordRateLimiterEndpoint(http.MethodPost, "route")
	global := discordRateLimiterEndpoint(http.MethodGet, "global")
	for _, endpoint := range []*rest.CompiledEndpoint{route, global} {
		if err := l.Wait(t.Context(), endpoint); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Unlock(route, discordRateLimiterResponse(http.StatusOK, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset-After": "0.06"})); err != nil {
		t.Fatal(err)
	}
	reset, _, _, _ := l.BucketForTest(route)
	if err := l.Unlock(global, discordRateLimiterResponse(http.StatusTooManyRequests, map[string]string{"Retry-After": "0.01", "X-RateLimit-Global": "true"})); err != nil {
		t.Fatal(err)
	}
	if err := l.Wait(t.Context(), route); err != nil {
		t.Fatal(err)
	}
	if time.Now().Before(reset) {
		t.Fatal("shorter global deadline bypassed route limit")
	}
	if err := l.Unlock(route, nil); err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for i := range 16 {
		workers.Go(func() {
			endpoint := discordRateLimiterEndpoint(http.MethodPost, fmt.Sprint(i))
			for range 8 {
				if err := l.Wait(t.Context(), endpoint); err != nil {
					t.Error(err)
					return
				}
				if err := l.Unlock(endpoint, discordRateLimiterResponse(http.StatusTooManyRequests, map[string]string{"Retry-After": "0.001", "X-RateLimit-Global": "true"})); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	workers.Wait()
}

func TestDiscordRateLimiterGlobalExtensionWakesWaitingRoutes(t *testing.T) {
	l := discord.NewDiscordRateLimiter(t.Context())
	defer l.Close(t.Context())
	route := discordRateLimiterEndpoint(http.MethodPost, "waiting")
	global := discordRateLimiterEndpoint(http.MethodGet, "global")
	for _, endpoint := range []*rest.CompiledEndpoint{route, global} {
		if err := l.Wait(t.Context(), endpoint); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Unlock(route, discordRateLimiterResponse(http.StatusTooManyRequests, map[string]string{"Retry-After": "0.06"})); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- l.Wait(t.Context(), route) }()
	waitIngressCondition(t, func() bool {
		_, _, references, busy := l.BucketForTest(route)
		return references == 1 && busy
	})
	if err := l.Unlock(global, discordRateLimiterResponse(http.StatusTooManyRequests, map[string]string{"Retry-After": "0.1", "X-RateLimit-Scope": "global"})); err != nil {
		t.Fatal(err)
	}
	reset := l.GlobalResetForTest()
	select {
	case err := <-done:
		if err != nil || time.Now().Before(reset) {
			t.Fatal("waiting route ignored a new global limit", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("global wait stalled")
	}
	if err := l.Unlock(route, nil); err != nil {
		t.Fatal(err)
	}
}

func TestDiscordRateLimiterRetryDelayParsingAndRelease(t *testing.T) {
	for _, value := range []string{"1", "0.125", "0.000000001"} {
		t.Run(value, func(t *testing.T) {
			l := discord.NewDiscordRateLimiter(t.Context())
			defer l.Close(t.Context())
			e := discordRateLimiterEndpoint(http.MethodPost, "route")
			if err := l.Wait(t.Context(), e); err != nil {
				t.Fatal(err)
			}
			before := time.Now()
			if err := l.Unlock(e, discordRateLimiterResponse(http.StatusTooManyRequests, map[string]string{"Retry-After": value})); err != nil {
				t.Fatal(err)
			}
			want, _ := discord.RateLimitSeconds(value)
			reset, _, _, _ := l.BucketForTest(e)
			got := reset.Sub(before)
			if got < want || got > want+time.Second {
				t.Fatal(got, want)
			}
		})
	}
	for _, value := range []string{"", "0", "-1", "NaN", "Inf", "1e30", "not-a-number"} {
		t.Run("invalid/"+value, func(t *testing.T) {
			l := discord.NewDiscordRateLimiter(t.Context())
			defer l.Close(t.Context())
			e := discordRateLimiterEndpoint(http.MethodPost, "route")
			if err := l.Wait(t.Context(), e); err != nil {
				t.Fatal(err)
			}
			if err := l.Unlock(e, discordRateLimiterResponse(http.StatusTooManyRequests, map[string]string{"Retry-After": value})); !errors.Is(err, discord.ErrDiscordRateLimitHeaders) {
				t.Fatal("invalid delay accepted", err)
			}
			if err := l.Wait(t.Context(), e); err != nil {
				t.Fatal("invalid delay leaked route lock", err)
			}
			if err := l.Unlock(e, nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDiscordRateLimiterCancellationReleasesDelayAndParentWait(t *testing.T) {
	parent, cancelParent := context.WithCancel(t.Context())
	l := discord.NewDiscordRateLimiter(parent)
	defer l.Close(t.Context())
	e := discordRateLimiterEndpoint(http.MethodPost, "route")
	if err := l.Wait(t.Context(), e); err != nil {
		t.Fatal(err)
	}
	if err := l.Unlock(e, discordRateLimiterResponse(http.StatusTooManyRequests, map[string]string{"Retry-After": "3600"})); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	if err := l.Wait(ctx, e); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	_, _, references, busy := l.BucketForTest(e)
	if busy || references != 0 {
		t.Fatal("cancelled delay retained bucket", busy, references)
	}
	done := make(chan error, 1)
	go func() { done <- l.Wait(t.Context(), e) }()
	waitIngressCondition(t, func() bool {
		_, _, references, busy := l.BucketForTest(e)
		return references == 1 && busy
	})
	cancelParent()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("parent cancellation not propagated", err)
	}
}

func TestDiscordRateLimiterResetWakesDelayAndCloseCancelsGlobalWait(t *testing.T) {
	l := discord.NewDiscordRateLimiter(t.Context())
	defer l.Close(t.Context())
	e := discordRateLimiterEndpoint(http.MethodPost, "route")
	for _, closeLimiter := range []bool{false, true} {
		if err := l.Wait(t.Context(), e); err != nil {
			t.Fatal(err)
		}
		if err := l.Unlock(e, discordRateLimiterResponse(http.StatusTooManyRequests, map[string]string{"Retry-After": "3600", "X-RateLimit-Global": "true"})); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- l.Wait(t.Context(), e) }()
		waitIngressCondition(t, func() bool {
			_, _, references, busy := l.BucketForTest(e)
			return references == 1 && busy
		})
		if closeLimiter {
			l.Close(t.Context())
		} else {
			l.Reset()
		}
		select {
		case err := <-done:
			if closeLimiter && !errors.Is(err, context.Canceled) || !closeLimiter && err != nil {
				t.Fatal("global wait retained after reset/close", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("global wait did not stop")
		}
		if !closeLimiter {
			if err := l.Unlock(e, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestDiscordRateLimiterConcurrentCloseCancelsWaitersAndResetKeepsActiveBucket(t *testing.T) {
	l := discord.NewDiscordRateLimiter(t.Context())
	e := discordRateLimiterEndpoint(http.MethodPost, "route")
	if err := l.Wait(t.Context(), e); err != nil {
		t.Fatal(err)
	}
	l.Reset()
	var waiters sync.WaitGroup
	errorsSeen := make(chan error, 32)
	for range 32 {
		waiters.Go(func() { errorsSeen <- l.Wait(t.Context(), e) })
	}
	waitIngressCondition(t, func() bool {
		_, _, references, _ := l.BucketForTest(e)
		return references == 33
	})
	var closers sync.WaitGroup
	for range 8 {
		closers.Go(func() { l.Close(t.Context()) })
	}
	closers.Wait()
	waiters.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if !errors.Is(err, context.Canceled) {
			t.Fatal("close did not cancel route waiter", err)
		}
	}
	if err := l.Unlock(e, nil); err != nil {
		t.Fatal(err)
	}
	l.Reset()
	if err := l.Wait(t.Context(), e); !errors.Is(err, context.Canceled) {
		t.Fatal("Reset reopened closed limiter", err)
	}
}

func TestDiscordRateLimiterBoundedBucketsNeverEvictsActiveRoutes(t *testing.T) {
	l := discord.NewDiscordRateLimiter(t.Context())
	defer l.Close(t.Context())
	for i := range discord.DiscordRateLimiterCapacity {
		if err := l.Wait(t.Context(), discordRateLimiterEndpoint(http.MethodPost, fmt.Sprint(i))); err != nil {
			t.Fatal(err)
		}
	}
	newRoute := discordRateLimiterEndpoint(http.MethodPost, "extra")
	if err := l.Wait(t.Context(), newRoute); !errors.Is(err, discord.ErrDiscordRateLimiterCapacity) {
		t.Fatal("active route evicted", err)
	}
	if err := l.Unlock(discordRateLimiterEndpoint(http.MethodPost, "0"), nil); err != nil {
		t.Fatal(err)
	}
	if err := l.Wait(t.Context(), newRoute); err != nil {
		t.Fatal("unused route not reclaimed", err)
	}
	count := l.BucketCountForTest()
	if count != discord.DiscordRateLimiterCapacity {
		t.Fatal("bucket limit exceeded", count)
	}
	if err := l.Unlock(newRoute, nil); err != nil {
		t.Fatal(err)
	}
}

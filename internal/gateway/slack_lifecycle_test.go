package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	slackapi "github.com/slack-go/slack"

	"github.com/keakon/chord-gateway/internal/im"
	"github.com/keakon/chord-gateway/internal/platform/slack"
)

func slackLifecycleAdapter(t *testing.T) (*slack.SlackAdapter, slack.Identity) {
	t.Helper()
	a, identity := slackSocketTestAdapter(t)
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	installation := strings.ToUpper(hex.EncodeToString(random[:]))
	identity.AppID = "ALIFECYCLE" + installation
	identity.BotUserID = "ULIFECYCLE" + installation
	identity.AccountID, _ = im.EncodeScopedID("account-v1:", identity.TeamID, identity.BotUserID)
	a.SetIdentityForTest(identity, true)
	a.SetVerifyIdentityForTest(func(context.Context) (slack.Identity, error) { return identity, nil })
	return a, identity
}

func connectSlackLifecycle(t *testing.T, a *slack.SlackAdapter) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- a.Connect() }()
	t.Cleanup(a.Disconnect)
	return done
}

func disconnectSlackLifecycle(t *testing.T, a *slack.SlackAdapter, done <-chan error) {
	t.Helper()
	stopped := make(chan struct{})
	go func() { a.Disconnect(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Disconnect did not join the supervisor and its workers")
	}
	if err := waitSlackSocketDone(t, done); err != nil {
		t.Fatal("intentional disconnect returned an error", err)
	}
	ready := a.ReadyForTest()
	conn, _ := a.SocketStateForTest()
	if ready || conn != nil {
		t.Fatal("disconnected supervisor retained ready socket")
	}
}

func TestSlackLifecycleReceiveLocksAcrossAccountsAndStateDirectories(t *testing.T) {
	for _, sameAccount := range []bool{false, true} {
		t.Run(fmt.Sprintf("same_account_%v", sameAccount), func(t *testing.T) {
			first, identity := slackLifecycleAdapter(t)
			opened := make(chan struct{})
			first.SetOpenSocketForTest(func(ctx context.Context) (string, error) {
				close(opened)
				<-ctx.Done()
				return "", ctx.Err()
			})
			firstDone := connectSlackLifecycle(t, first)
			select {
			case <-opened:
			case <-time.After(time.Second):
				t.Fatal("first supervisor did not acquire locks")
			}
			second, secondIdentity := slackLifecycleAdapter(t)
			secondIdentity.AppID = identity.AppID
			if sameAccount {
				secondIdentity = identity
			} else {
				secondIdentity.TeamID = "TOTHER"
				secondIdentity.AccountID, _ = im.EncodeScopedID("account-v1:", secondIdentity.TeamID, secondIdentity.BotUserID)
			}
			second.SetIdentityForTest(secondIdentity, true)
			second.SetVerifyIdentityForTest(func(context.Context) (slack.Identity, error) { return secondIdentity, nil })
			second.SetOpenSocketForTest(func(context.Context) (string, error) {
				t.Error("second receiver opened a socket despite receive-lock conflict")
				return "", slack.ErrFatal
			})
			if first.HostForTest().(*NotificationRouter).mgr.pins.path == second.HostForTest().(*NotificationRouter).mgr.pins.path {
				t.Fatal("fixture reused state directory")
			}
			if err := second.Connect(); !errors.Is(err, im.ErrInstanceRunning) {
				t.Fatal("second supervisor did not reject account/app contention", err)
			}
			disconnectSlackLifecycle(t, first, firstDone)
			// A fresh supervisor can reacquire after the owner has fully stopped.
			third, _ := slackLifecycleAdapter(t)
			third.SetIdentityForTest(identity, true)
			third.SetVerifyIdentityForTest(func(context.Context) (slack.Identity, error) { return identity, nil })
			openedAgain := make(chan struct{})
			third.SetOpenSocketForTest(func(ctx context.Context) (string, error) {
				close(openedAgain)
				<-ctx.Done()
				return "", ctx.Err()
			})
			thirdDone := connectSlackLifecycle(t, third)
			select {
			case <-openedAgain:
			case <-time.After(time.Second):
				t.Fatal("receive locks were not released after Disconnect")
			}
			disconnectSlackLifecycle(t, third, thirdDone)
		})
	}
}

func TestSlackLifecycleDisconnectCancelsStartupAndBackoff(t *testing.T) {
	for _, stage := range []string{"auth_api", "socket_open", "socket_dial", "backoff"} {
		t.Run(stage, func(t *testing.T) {
			a, identity := slackLifecycleAdapter(t)
			started := make(chan struct{})
			switch stage {
			case "auth_api":
				release := make(chan struct{})
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_ = r.ParseForm()
					close(started)
					select {
					case <-r.Context().Done():
					case <-release:
					}
				}))
				t.Cleanup(func() { close(release); server.Close() })
				client := slackapi.New("test-token", slackapi.OptionAPIURL(server.URL+"/"), slackapi.OptionHTTPClient(slack.NewHTTPClient(server.Client(), nil)))
				a.SetVerifyIdentityForTest(func(ctx context.Context) (slack.Identity, error) {
					return slack.VerifyIdentity(ctx, client, identity.TeamID, identity.AppID)
				})
			case "socket_open":
				a.SetSocketHooksForTest(func(ctx context.Context) (string, error) {
					close(started)
					<-ctx.Done()
					return "", ctx.Err()
				}, nil)
			case "socket_dial":
				a.SetSocketHooksForTest(func(context.Context) (string, error) { return "ws://test.invalid", nil }, func(ctx context.Context, address string) (*websocket.Conn, error) {
					close(started)
					<-ctx.Done()
					return nil, ctx.Err()
				})
			case "backoff":
				a.SetVerifyIdentityForTest(func(context.Context) (slack.Identity, error) {
					close(started)
					return slack.Identity{}, errors.New("temporary API failure")
				})
			}
			done := connectSlackLifecycle(t, a)
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("supervisor did not enter expected wait")
			}
			if stage == "backoff" {
				// The first retry sleeps at least 800ms. Allow startup to leave
				// verifyIdentity before interrupting that cancellable wait.
				time.Sleep(25 * time.Millisecond)
			}
			disconnectSlackLifecycle(t, a, done)
		})
	}
}

func TestSlackLifecycleRefreshRechecksHelloAndJoinsOldGeneration(t *testing.T) {
	a, identity := slackLifecycleAdapter(t)
	connections := make(chan *websocket.Conn, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err == nil {
			connections <- conn
		}
	}))
	t.Cleanup(server.Close)
	var opens, verifications atomic.Int32
	a.SetSocketHooksForTest(func(context.Context) (string, error) {
		count := opens.Add(1)
		return "ws" + strings.TrimPrefix(server.URL, "http") + fmt.Sprintf("/?ticket=%d", count), nil
	}, func(ctx context.Context, address string) (*websocket.Conn, error) {
		conn, _, err := websocket.DefaultDialer.DialContext(ctx, address, nil)
		return conn, err
	})
	a.SetVerifyIdentityForTest(func(context.Context) (slack.Identity, error) {
		verifications.Add(1)
		return identity, nil
	})
	done := connectSlackLifecycle(t, a)
	var first *websocket.Conn
	select {
	case first = <-connections:
	case <-time.After(time.Second):
		t.Fatal("initial socket not dialed")
	}
	t.Cleanup(func() { _ = first.Close() })
	if err := first.WriteJSON(map[string]any{"type": "hello", "connection_info": map[string]string{"app_id": identity.AppID}}); err != nil {
		t.Fatal(err)
	}
	waitIngressCondition(t, func() bool { return a.ReadyForTest() })
	_, oldGeneration := a.SocketStateForTest()
	if err := first.WriteJSON(map[string]string{"type": "disconnect", "reason": "refresh_requested"}); err != nil {
		t.Fatal(err)
	}
	var second *websocket.Conn
	select {
	case second = <-connections:
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor did not request fresh connection after refresh")
	}
	t.Cleanup(func() { _ = second.Close() })
	waitIngressCondition(t, func() bool {
		conn, generation := a.SocketStateForTest()
		return generation > oldGeneration && conn != nil
	})
	ready := a.ReadyForTest()
	_, currentGeneration := a.SocketStateForTest()
	if ready || currentGeneration <= oldGeneration || opens.Load() != 2 || verifications.Load() != 3 {
		t.Fatal("new connection inherited old readiness or skipped identity verification", ready, currentGeneration, opens.Load(), verifications.Load())
	}
	if err := second.WriteJSON(map[string]any{"type": "hello", "connection_info": map[string]string{"app_id": identity.AppID}}); err != nil {
		t.Fatal(err)
	}
	waitIngressCondition(t, func() bool { return a.ReadyForTest() })
	if verifications.Load() != 4 {
		t.Fatal("new hello did not cause a fresh auth identity check", verifications.Load())
	}
	// The old server endpoint must observe the closed generation. No worker
	// may retain that socket once the supervisor dials the replacement.
	_ = first.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := first.ReadMessage(); err == nil {
		t.Fatal("old generation remained readable after replacement dial")
	}
	// A stale completion submitted to a writer for the new connection cannot
	// become a confirmation on the new socket.
	currentConn, _ := a.SocketStateForTest()
	ctx, cancel := context.WithCancel(t.Context())
	queue := make(chan slack.ACK, 1)
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		a.ACKWriterForTest(ctx, currentConn, currentGeneration, queue, func(error) { t.Error("stale ACK caused socket write") })
	}()
	request := slack.ACK{Generation: oldGeneration, Deadline: time.Now().Add(time.Second), Data: []byte(`{"envelope_id":"old-generation"}`), Done: make(chan error, 1)}
	queue <- request
	select {
	case err := <-request.Done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("stale generation completion was accepted", err)
		}
	case <-time.After(time.Second):
		t.Fatal("stale ACK did not get a completion")
	}
	cancel()
	<-writerDone
	disconnectSlackLifecycle(t, a, done)
	_ = second.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := second.ReadMessage(); err == nil {
		t.Fatal("Disconnect left the current socket open")
	}
}

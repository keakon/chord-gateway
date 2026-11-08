package slack

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// dialSlackSocketWithProxy collapses every dial failure into the sanitized
// socket_dial_failed category and closes the rejected handshake body.
func TestDialSlackSocketWithProxyErrorPaths(t *testing.T) {
	t.Run("refused connection", func(t *testing.T) {
		conn, err := dialSlackSocketWithProxy(t.Context(), "wss://127.0.0.1:1/", nil)
		if conn != nil {
			t.Fatal("dial returned a connection on a refused port")
		}
		if err != (ConnectionError{category: "socket_dial_failed"}) {
			t.Fatalf("dial error = %v, want socket_dial_failed", err)
		}
	})

	t.Run("rejected handshake closes the response body", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "forbidden", http.StatusForbidden)
		}))
		defer server.Close()
		serverURL := &url.URL{Scheme: "ws", Host: server.Listener.Addr().String()}

		conn, err := dialSlackSocketWithProxy(t.Context(), serverURL.String(), nil)
		if conn != nil {
			t.Fatal("dial returned a connection for a rejected handshake")
		}
		if err != (ConnectionError{category: "socket_dial_failed"}) {
			t.Fatalf("dial error = %v, want socket_dial_failed", err)
		}
	})

	t.Run("proxy failure", func(t *testing.T) {
		proxyErr := context.Canceled
		proxy := func(*http.Request) (*url.URL, error) { return nil, proxyErr }
		conn, err := dialSlackSocketWithProxy(t.Context(), "wss://wss.slack.com/", proxy)
		if conn != nil {
			t.Fatal("dial returned a connection on a proxy failure")
		}
		if err != (ConnectionError{category: "socket_dial_failed"}) {
			t.Fatalf("dial error = %v, want socket_dial_failed", err)
		}
	})
}

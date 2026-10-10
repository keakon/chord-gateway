package main

import (
	"errors"
	"net/http"
)

// An unconfirmed message request must not be replayed: the platform may already
// have delivered it even when the receipt is absent or the connection fails.
var errDeliveryOutcomeUnknown = errors.New("message delivery outcome is unknown")

// A 307/308 redirect can replay a POST body. Stop before following it, even
// when the redirect points at the same host, and let the caller report unknown.
func messageHTTPClient(client *http.Client) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	copy := *client
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &copy
}

// isAmbiguousDeliveryStatus reports whether an HTTP status leaves the delivery
// outcome unknown. An ordinary 4xx is a definite rejection; 408/429 remain
// conservative unknown outcomes. Unexpected statuses are also unknown, since
// neither an alternate 2xx nor a redirect proves rejection. HTTP 200 still
// requires the caller to validate the platform receipt.
func isAmbiguousDeliveryStatus(code int) bool {
	switch code {
	case http.StatusOK:
		return false
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		return true
	}
	return code < http.StatusBadRequest || code >= http.StatusInternalServerError
}

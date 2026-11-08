// Package feishu unit tests for WebSocket handshake error classification.
package feishu

import (
	"net/http"
	"testing"

	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

func TestParseFeishuWSError(t *testing.T) {
	const handshakeMsg = "connection handshake rejected"

	cases := []struct {
		name            string
		handshakeStatus string
		authErrCode     string
		respStatusCode  int
		respStatus      string
		wantClient      bool
		wantCode        int
		wantMsg         string
	}{
		{
			name:            "auth failed exceed conn limit is client error",
			handshakeStatus: "514",
			authErrCode:     "1000040350",
			wantClient:      true,
			wantCode:        larkws.AuthFailed,
			wantMsg:         handshakeMsg,
		},
		{
			name:            "auth failed other auth code is server error",
			handshakeStatus: "514",
			authErrCode:     "10003",
			wantClient:      false,
			wantCode:        larkws.AuthFailed,
			wantMsg:         handshakeMsg,
		},
		{
			name:            "auth failed missing auth code is server error",
			handshakeStatus: "514",
			wantClient:      false,
			wantCode:        larkws.AuthFailed,
			wantMsg:         handshakeMsg,
		},
		{
			name:            "forbidden is client error",
			handshakeStatus: "403",
			wantClient:      true,
			wantCode:        larkws.Forbidden,
			wantMsg:         handshakeMsg,
		},
		{
			name:            "other handshake status is server error",
			handshakeStatus: "500",
			wantClient:      false,
			wantCode:        500,
			wantMsg:         handshakeMsg,
		},
		{
			name:           "missing handshake status falls back to http status",
			respStatusCode: 502,
			respStatus:     "502 Bad Gateway",
			wantClient:     false,
			wantCode:       502,
			wantMsg:        "502 Bad Gateway",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{
				Header:     make(http.Header),
				StatusCode: tc.respStatusCode,
				Status:     tc.respStatus,
			}
			if tc.handshakeStatus != "" {
				resp.Header.Set(larkws.HeaderHandshakeStatus, tc.handshakeStatus)
			}
			if tc.authErrCode != "" {
				resp.Header.Set(larkws.HeaderHandshakeAuthErrCode, tc.authErrCode)
			}

			err := parseFeishuWSError(resp)
			if err == nil {
				t.Fatal("parseFeishuWSError returned nil, want error")
			}
			if tc.wantClient {
				clientErr, ok := err.(*larkws.ClientError)
				if !ok {
					t.Fatalf("parseFeishuWSError returned %T, want *larkws.ClientError", err)
				}
				if clientErr.Code != tc.wantCode {
					t.Errorf("client error code = %d, want %d", clientErr.Code, tc.wantCode)
				}
				if clientErr.Msg != tc.wantMsg {
					t.Errorf("client error msg = %q, want %q", clientErr.Msg, tc.wantMsg)
				}
				return
			}
			serverErr, ok := err.(*larkws.ServerError)
			if !ok {
				t.Fatalf("parseFeishuWSError returned %T, want *larkws.ServerError", err)
			}
			if serverErr.Code != tc.wantCode {
				t.Errorf("server error code = %d, want %d", serverErr.Code, tc.wantCode)
			}
			if serverErr.Msg != tc.wantMsg {
				t.Errorf("server error msg = %q, want %q", serverErr.Msg, tc.wantMsg)
			}
		})
	}
}

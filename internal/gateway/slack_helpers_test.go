package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	slackapi "github.com/slack-go/slack"

	"github.com/keakon/chord-gateway/internal/platform/slack"
)

func testSlackAPI(t *testing.T, handler http.HandlerFunc) *slackapi.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return slackapi.New("sample-bot-token", slackapi.OptionAPIURL(server.URL+"/"), slackapi.OptionHTTPClient(slack.NewHTTPClient(server.Client(), nil)), slackapi.OptionDebug(false))
}

func testSlackSender(t *testing.T, client *slackapi.Client) *slack.Sender {
	t.Helper()
	sender, err := slack.NewSender(client)
	if err != nil {
		t.Fatal(err)
	}
	return sender
}

package main

import "testing"

func TestRedactProxyURL(t *testing.T) {
	cases := map[string]string{
		"http://user:secret@127.0.0.1:1087": "http://127.0.0.1:1087",
		"socks5h://127.0.0.1:1080":          "socks5h://127.0.0.1:1080",
		"http:///missing-host":              "(configured)",
		"not a url":                         "(configured)",
	}
	for raw, want := range cases {
		if got := redactProxyURL(raw); got != want {
			t.Errorf("redactProxyURL(%q) = %q, want %q", raw, got, want)
		}
	}
}

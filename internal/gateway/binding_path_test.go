package gateway

import (
	"testing"

	"github.com/keakon/chord-gateway/config"
)

func TestIsAllowedWorkspacePath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{path: "/work/project", want: true},
		{path: "~/work/project", want: true},
		{path: "~", want: true},
		{path: `C:\work\project`, want: true},
		{path: "D:/work/project", want: true},
		{path: `\\server\share\project`, want: true},
		{path: "./project", want: false},
		{path: "project", want: false},
		{path: `C:relative\project`, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := config.IsAllowedWorkspacePath(tt.path); got != tt.want {
				t.Fatalf("IsAllowedWorkspacePath(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

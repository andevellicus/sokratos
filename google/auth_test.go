package google

import (
	"errors"
	"testing"
)

func TestIsAuthError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"unrelated error", errors.New("connection refused"), false},
		{"invalid_grant", errors.New(`Get "https://...": oauth2: "invalid_grant" "Token has been expired or revoked."`), true},
		{"token expired", errors.New("token expired"), true},
		{"cannot fetch token", errors.New("oauth2: cannot fetch token: 400 Bad Request"), true},
		{"wrapped invalid_grant", errors.New(`list messages: Get "...": oauth2: "invalid_grant"`), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsAuthError(tt.err)
			if got != tt.want {
				t.Errorf("IsAuthError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

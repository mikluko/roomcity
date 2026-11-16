package sessionauth

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTokenReadsABearerHeader(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   string
	}{
		{"bearer", "Bearer abc", "abc"},
		{"no header", "", ""},
		{"another scheme", "Basic abc", ""},
		{"scheme only", "Bearer ", ""},
		{"lower case scheme", "bearer abc", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			if tt.header != "" {
				h.Set("Authorization", tt.header)
			}
			require.Equal(t, tt.want, Token(h))
		})
	}
}

func TestSetRoundTrips(t *testing.T) {
	h := http.Header{}
	Set(h, "tok")
	require.Equal(t, "Bearer tok", h.Get("Authorization"))
	require.Equal(t, "tok", Token(h))
}

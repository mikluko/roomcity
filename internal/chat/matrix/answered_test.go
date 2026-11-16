package matrix

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix"

	"sarak.sh/mikluko/roomcity/internal/chat"
)

func TestAnsweredTellsARefusalFromAHomeserverOutOfReach(t *testing.T) {
	status := func(code int, errcode string) error {
		return fmt.Errorf("sending: %w", mautrix.HTTPError{
			Response:  &http.Response{StatusCode: code},
			RespError: &mautrix.RespError{ErrCode: errcode},
		})
	}
	tests := []struct {
		name    string
		err     error
		refused bool
	}{
		{"a reaction it already holds", status(400, "M_DUPLICATE_ANNOTATION"), true},
		{"a room it will not let the user write in", status(403, "M_FORBIDDEN"), true},
		{"a rate limit, asked again", status(429, "M_LIMIT_EXCEEDED"), false},
		{"a timeout, asked again", status(408, ""), false},
		{"a homeserver that does not know the registration", status(401, "M_UNKNOWN_TOKEN"), false},
		{"a homeserver that is failing", status(502, ""), false},
		{"a connection that failed", errors.New("dial tcp: connection refused"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.refused, chat.IsRefused(answered(tt.err)))
		})
	}
}

// Package sessionauth is how a session proves itself to the server: the
// server makes a token for each session and puts it in the session's pod,
// under EnvToken, and the host and the stand-in gc send it with every call
// as a bearer token.
package sessionauth

import (
	"context"
	"net/http"
	"strings"

	"connectrpc.com/connect"
)

// EnvToken names the variable of a session's environment that holds its
// token.
const EnvToken = "ROOMCITY_TOKEN"

const scheme = "Bearer "

// Set puts token in h as the session's bearer token.
func Set(h http.Header, token string) {
	h.Set("Authorization", scheme+token)
}

// Token returns the bearer token h carries, or "" when it carries none.
func Token(h http.Header) string {
	v := h.Get("Authorization")
	if !strings.HasPrefix(v, scheme) {
		return ""
	}
	return strings.TrimPrefix(v, scheme)
}

// Client returns the client option that sends token with every call.
func Client(token string) connect.ClientOption {
	return connect.WithInterceptors(sender(token))
}

type sender string

func (s sender) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		Set(req.Header(), string(s))
		return next(ctx, req)
	}
}

func (s sender) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		Set(conn.RequestHeader(), string(s))
		return conn
	}
}

func (sender) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

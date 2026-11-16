package server

import (
	"net/http"

	"sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1/roomcityv1connect"
	"sarak.sh/mikluko/roomcity/internal/sessionauth"
)

// sendOverhead is what a Send request holds beside the file: the session's
// name, the file's name, its media type and caption, and the framing.
const sendOverhead = 64 << 10

// TokenChecker says whether a bearer token is some live session's.
type TokenChecker interface {
	KnownToken(token string) bool
}

// LimitSend wraps the network handler so that a Send, which carries a file, is
// refused before its body is read when it carries no token of a session that
// has not ended, and is read no further than the largest file takes. Without
// it any pod of the namespace could make the server read as much as it was
// sent, since the call's own check of the token comes after the body is read.
// Other calls pass as they are.
func LimitSend(next http.Handler, tokens TokenChecker, maxFile int64) http.Handler {
	if maxFile <= 0 {
		maxFile = DefaultMaxFile
	}
	// A file is bytes in the message, and in JSON base64 of them: a third more.
	limit := maxFile/3*4 + 4 + sendOverhead
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != roomcityv1connect.HostServiceSendProcedure {
			next.ServeHTTP(w, r)
			return
		}
		if !tokens.KnownToken(sessionauth.Token(r.Header)) {
			http.Error(w, "the session's token is missing or not a live session's", http.StatusUnauthorized)
			return
		}
		if r.ContentLength > limit {
			http.Error(w, "the file is larger than the server takes", http.StatusRequestEntityTooLarge)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		next.ServeHTTP(w, r)
	})
}

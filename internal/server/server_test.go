package server_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"sarak.sh/mikluko/roomcity/internal/server"
)

func TestHandle(t *testing.T) {
	s := server.New()
	s.Handle("/a/", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/a/x", nil))
	require.Equal(t, http.StatusAccepted, rec.Code)

	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/b", nil))
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestHandleTwicePanics(t *testing.T) {
	s := server.New()
	s.Handle("/a/", http.NotFoundHandler())
	require.Panics(t, func() { s.Handle("/a/", http.NotFoundHandler()) })
}

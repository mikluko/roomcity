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

func TestAHandlerIsOnItsOwnListenerOnly(t *testing.T) {
	s := server.New()
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) })
	s.HandleRuntime("/r/", h)
	s.HandlePage("/p/", h)

	code := func(l http.Handler, path string) int {
		rec := httptest.NewRecorder()
		l.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Code
	}
	require.Equal(t, http.StatusAccepted, code(s.RuntimeHandler(), "/r/x"))
	require.Equal(t, http.StatusAccepted, code(s.PageHandler(), "/p/x"))
	for _, l := range []http.Handler{s.Handler(), s.PageHandler()} {
		require.Equal(t, http.StatusNotFound, code(l, "/r/x"))
	}
	for _, l := range []http.Handler{s.Handler(), s.RuntimeHandler()} {
		require.Equal(t, http.StatusNotFound, code(l, "/p/x"))
	}
}

func TestHandleTwicePanics(t *testing.T) {
	s := server.New()
	s.Handle("/a/", http.NotFoundHandler())
	require.Panics(t, func() { s.Handle("/a/", http.NotFoundHandler()) })
}

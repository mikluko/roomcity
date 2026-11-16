package standin

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"sarak.sh/mikluko/roomcity/internal/gchandler"
	"sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1/roomcityv1connect"
)

func authorize(session, token string) bool { return session != "" && token == "tok" }

// server serves GcService over a gc that is the script body.
func server(t *testing.T, body string) string {
	t.Helper()
	gc := filepath.Join(t.TempDir(), "gc")
	require.NoError(t, os.WriteFile(gc, []byte("#!/bin/sh\n"+body), 0o755))
	mux := http.NewServeMux()
	mux.Handle(roomcityv1connect.NewGcServiceHandler(gchandler.New(gchandler.Config{CityDir: t.TempDir(), GcPath: gc, Env: []string{}, Authorize: authorize})))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestRunReturnsWhatTheServersGcPrinted(t *testing.T) {
	url := server(t, `printf 'args:%s|stdin:%s|GC_X:%s|HOME:%s' "$*" "$(cat)" "$GC_X" "${HOME-unset}"; printf warn >&2; exit 4`)
	var stdout, stderr bytes.Buffer

	code := Run(context.Background(), Config{Session: "s", Token: "tok", Server: url},
		[]string{"nudge", "drain", "--inject"},
		Streams{
			Stdin:  strings.NewReader("payload"),
			Stdout: &stdout, Stderr: &stderr,
			Env: []string{"GC_X=1", "HOME=/home/city", "PATH=/bin"},
			Dir: "/home/city/work",
		})

	require.Equal(t, 4, code)
	require.Equal(t, "args:nudge drain --inject|stdin:payload|GC_X:1|HOME:unset", stdout.String())
	require.Equal(t, "warn", stderr.String())
}

func TestRunSendsTheSessionAndDirectory(t *testing.T) {
	var got struct{ session, cwd string }
	mux := http.NewServeMux()
	mux.Handle(roomcityv1connect.NewGcServiceHandler(spy{func(session, cwd string) { got.session, got.cwd = session, cwd }}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	code := Run(context.Background(), Config{Session: "worker-1", Token: "tok", Server: srv.URL}, nil,
		Streams{Stdin: strings.NewReader(""), Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}, Dir: "/work"})

	require.Equal(t, 0, code)
	require.Equal(t, "worker-1", got.session)
	require.Equal(t, "/work", got.cwd)
}

func TestRunWithNoServerFailsWithOne(t *testing.T) {
	var stderr bytes.Buffer

	code := Run(context.Background(), Config{Session: "s", Token: "tok", Server: "http://127.0.0.1:1"}, []string{"prime"},
		Streams{Stdin: strings.NewReader(""), Stdout: &bytes.Buffer{}, Stderr: &stderr})

	require.Equal(t, 1, code)
	require.Contains(t, stderr.String(), "gc: ")
}

func TestConfigFromEnv(t *testing.T) {
	cfg, err := ConfigFromEnv([]string{"ROOMCITY_SESSION=s", "ROOMCITY_TOKEN=tok", "ROOMCITY_SERVER=http://roomcity:8080", "PATH=/bin"})
	require.NoError(t, err)
	require.Equal(t, Config{Session: "s", Token: "tok", Server: "http://roomcity:8080"}, cfg)

	_, err = ConfigFromEnv([]string{"ROOMCITY_SESSION=s", "ROOMCITY_TOKEN=tok"})
	require.ErrorContains(t, err, "ROOMCITY_SERVER")
	_, err = ConfigFromEnv([]string{"ROOMCITY_SERVER=x", "ROOMCITY_TOKEN=tok"})
	require.ErrorContains(t, err, "ROOMCITY_SESSION")
	_, err = ConfigFromEnv([]string{"ROOMCITY_SERVER=x", "ROOMCITY_SESSION=s"})
	require.ErrorContains(t, err, "ROOMCITY_TOKEN")
}

func TestRunWithAWrongTokenIsRefused(t *testing.T) {
	url := server(t, `echo ran`)
	var stdout, stderr bytes.Buffer

	code := Run(context.Background(), Config{Session: "s", Token: "other", Server: url}, nil,
		Streams{Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr})

	require.Equal(t, 1, code)
	require.Empty(t, stdout.String())
	require.Contains(t, stderr.String(), "unauthenticated")
}

package gchandler

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	roomcityv1 "sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1"
	"sarak.sh/mikluko/roomcity/internal/sessionauth"
)

// fakeGc writes a gc that reports what it was run with.
func fakeGc(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gc")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755))
	return path
}

// token is the one session "s" holds.
const token = "tok"

func authorize(session, got string) bool { return session == "s" && got == token }

func request(msg *roomcityv1.RunRequest) *connect.Request[roomcityv1.RunRequest] {
	req := connect.NewRequest(msg)
	sessionauth.Set(req.Header(), token)
	return req
}

func run(t *testing.T, h *Handler, req *roomcityv1.RunRequest) *roomcityv1.RunResponse {
	t.Helper()
	res, err := h.Run(context.Background(), request(req))
	require.NoError(t, err)
	return res.Msg
}

func TestRunReturnsOutputAndExitCode(t *testing.T) {
	gc := fakeGc(t, `printf 'out:%s' "$*"; printf 'err' >&2; exit 3`)
	h := New(Config{CityDir: t.TempDir(), GcPath: gc, Authorize: authorize})

	res := run(t, h, &roomcityv1.RunRequest{Session: "s", Args: []string{"mail", "check"}})

	require.Equal(t, "out:mail check", string(res.Stdout))
	require.Equal(t, "err", string(res.Stderr))
	require.Equal(t, int32(3), res.ExitCode)
}

func TestRunPassesStdin(t *testing.T) {
	gc := fakeGc(t, `cat`)
	h := New(Config{CityDir: t.TempDir(), GcPath: gc, Authorize: authorize})

	res := run(t, h, &roomcityv1.RunRequest{Session: "s", Stdin: []byte("hook payload")})

	require.Equal(t, "hook payload", string(res.Stdout))
}

func TestRunsInTheCityDirectoryNotTheSessionsCwd(t *testing.T) {
	city := t.TempDir()
	gc := fakeGc(t, `pwd -P`)
	h := New(Config{CityDir: city, GcPath: gc, Authorize: authorize})

	res := run(t, h, &roomcityv1.RunRequest{Session: "s", Cwd: "/home/city/work"})

	want, err := filepath.EvalSymlinks(city)
	require.NoError(t, err)
	require.Equal(t, want, strings.TrimSpace(string(res.Stdout)))
}

func TestRunEnvironment(t *testing.T) {
	gc := fakeGc(t, `printf '%s|%s|%s' "$GC_SESSION_NAME" "$FROM_SERVER" "${NOT_GC-unset}"`)
	h := New(Config{CityDir: t.TempDir(), GcPath: gc, Authorize: authorize, Env: []string{"FROM_SERVER=yes", "GC_SESSION_NAME=server's"}})

	res := run(t, h, &roomcityv1.RunRequest{Session: "s", Env: map[string]string{
		"GC_SESSION_NAME": "session's",
		"NOT_GC":          "leaks",
	}})

	// The session's GC_* wins over the server's; anything else of the
	// session's is dropped.
	require.Equal(t, "session's|yes|unset", string(res.Stdout))
}

func TestRunFailsToStart(t *testing.T) {
	h := New(Config{CityDir: t.TempDir(), GcPath: filepath.Join(t.TempDir(), "absent"), Authorize: authorize})

	_, err := h.Run(context.Background(), request(&roomcityv1.RunRequest{Session: "s"}))

	require.Equal(t, connect.CodeInternal, connect.CodeOf(err))
}

func TestRunRefusesWhatIsNotTheSessionsOwnToken(t *testing.T) {
	ran := filepath.Join(t.TempDir(), "ran")
	gc := fakeGc(t, `touch `+ran)
	h := New(Config{CityDir: t.TempDir(), GcPath: gc, Authorize: authorize})

	missing := connect.NewRequest(&roomcityv1.RunRequest{Session: "s"})
	wrong := connect.NewRequest(&roomcityv1.RunRequest{Session: "s"})
	sessionauth.Set(wrong.Header(), "other")
	another := connect.NewRequest(&roomcityv1.RunRequest{Session: "t"})
	sessionauth.Set(another.Header(), token)

	for name, req := range map[string]*connect.Request[roomcityv1.RunRequest]{"missing": missing, "wrong": wrong, "another session's": another} {
		_, err := h.Run(context.Background(), req)
		require.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err), name)
	}
	require.NoFileExists(t, ran)
}

func TestNoAuthorizeRefusesEverything(t *testing.T) {
	h := New(Config{CityDir: t.TempDir(), GcPath: fakeGc(t, `true`)})

	_, err := h.Run(context.Background(), request(&roomcityv1.RunRequest{Session: "s"}))

	require.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err))
}

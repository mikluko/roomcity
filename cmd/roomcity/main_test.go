package main

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

func TestRun(t *testing.T) {
	tests := []struct {
		name   string
		argv   []string
		code   int
		stdout string
		stderr string
	}{
		{"no command", []string{"roomcity"}, exitUsage, "", usage},
		{"no argv", nil, exitUsage, "", usage},
		{"help", []string{"roomcity", "-h"}, exitOK, usage, ""},
		{"unknown command", []string{"roomcity", "attach"}, exitUsage, "", "roomcity: unknown command \"attach\"\n" + usage},
		{"runtime", []string{"roomcity", "runtime", "protocol"}, exitFailure, "", "roomcity runtime: not implemented\n"},
		{"server", []string{"roomcity", "server"}, exitFailure, "", "roomcity server: not implemented\n"},
		{"host", []string{"roomcity", "host"}, exitFailure, "", "roomcity host: ROOMCITY_SESSION is not set\n"},
		{"gc", []string{"roomcity", "gc", "prime"}, exitFailure, "", "gc: ROOMCITY_SESSION is not set\n"},
		{"as gc-runtime-room", []string{"/usr/local/bin/gc-runtime-room", "start", "host"}, exitFailure, "", "roomcity runtime: not implemented\n"},
		{"as gc", []string{"/usr/local/lib/roomcity/bin/gc", "hook"}, exitFailure, "", "gc: ROOMCITY_SESSION is not set\n"},
		{"as gc, no arguments", []string{"gc"}, exitFailure, "", "gc: ROOMCITY_SESSION is not set\n"},
		{"as gc, asked for help", []string{"gc", "-h"}, exitFailure, "", "gc: ROOMCITY_SESSION is not set\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), tt.argv, process{Stdout: &stdout, Stderr: &stderr})
			require.Equal(t, tt.code, code)
			require.Equal(t, tt.stdout, stdout.String())
			require.Equal(t, tt.stderr, stderr.String())
		})
	}
}

func TestStandInGcEndsWithTheServersGcCode(t *testing.T) {
	gc := filepath.Join(t.TempDir(), "gc")
	require.NoError(t, os.WriteFile(gc, []byte("#!/bin/sh\nprintf 'ran %s' \"$*\"; exit 7\n"), 0o755))
	mux := http.NewServeMux()
	mux.Handle(roomcityv1connect.NewGcServiceHandler(gchandler.New(gchandler.Config{CityDir: t.TempDir(), GcPath: gc, Authorize: func(session, token string) bool { return session == "s" && token == "tok" }})))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	var stdout, stderr bytes.Buffer

	code := run(context.Background(), []string{"/usr/local/lib/roomcity/bin/gc", "prime", "--hook"}, process{
		Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr,
		Env: []string{"ROOMCITY_SESSION=s", "ROOMCITY_TOKEN=tok", "ROOMCITY_SERVER=" + srv.URL},
	})

	require.Equal(t, 7, code)
	require.Equal(t, "ran prime --hook", stdout.String())
}

package main

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/require"
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
		{"host", []string{"roomcity", "host"}, exitFailure, "", "roomcity host: not implemented\n"},
		{"gc", []string{"roomcity", "gc", "prime"}, exitFailure, "", "roomcity gc: not implemented\n"},
		{"as gc-runtime-room", []string{"/usr/local/bin/gc-runtime-room", "start", "host"}, exitFailure, "", "roomcity runtime: not implemented\n"},
		{"as gc", []string{"/usr/local/lib/roomcity/bin/gc", "hook"}, exitFailure, "", "roomcity gc: not implemented\n"},
		{"as gc, no arguments", []string{"gc"}, exitFailure, "", "roomcity gc: not implemented\n"},
		{"as gc, asked for help", []string{"gc", "-h"}, exitFailure, "", "roomcity gc: not implemented\n"},
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

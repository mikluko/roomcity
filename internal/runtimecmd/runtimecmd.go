// Package runtimecmd is gc-runtime-room: Gas City's exec session runtime
// command, which hands one operation to the server and returns what the
// server answers.
package runtimecmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"

	roomcityv1 "sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1"
	"sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1/roomcityv1connect"
)

// EnvURL names the variable that holds the address of the server's runtime
// listener.
const EnvURL = "ROOMCITY_RUNTIME_URL"

// callTimeout bounds a call: gc's own longest deadline is 120 seconds, for
// start, and it ends the command itself when that passes.
const callTimeout = 125 * time.Second

// Streams are the command's standard input and outputs.
type Streams struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
}

// Run carries out the operation args names, args[0] being the operation and
// the rest its arguments, and returns the exit code to end with. Anything
// that keeps the server from answering is exit code 1, never 2, which gc
// reads as an operation the runtime does not know and treats as success.
// env is as os.Environ gives it: the server's address and the GC_*
// variables are taken from it.
func Run(ctx context.Context, args []string, env []string, s Streams) int {
	if len(args) == 0 {
		return failf(s, "no operation")
	}
	var url string
	vars := map[string]string{}
	for _, kv := range env {
		name, value, _ := strings.Cut(kv, "=")
		switch {
		case name == EnvURL:
			url = value
		case strings.HasPrefix(name, "GC_"):
			vars[name] = value
		}
	}
	if url == "" {
		return failf(s, "%s is not set", EnvURL)
	}
	stdin, err := readStdin(s.Stdin)
	if err != nil {
		return failf(s, "reading standard input: %v", err)
	}

	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	client := roomcityv1connect.NewRuntimeServiceClient(http.DefaultClient, url)
	res, err := client.Operate(ctx, connect.NewRequest(&roomcityv1.OperateRequest{
		Operation: args[0],
		Args:      args[1:],
		Stdin:     stdin,
		Env:       vars,
	}))
	if err != nil {
		return failf(s, "%s: %v", args[0], err)
	}
	_, _ = s.Stdout.Write(res.Msg.Stdout)
	_, _ = s.Stderr.Write(res.Msg.Stderr)
	return int(res.Msg.ExitCode)
}

func failf(s Streams, format string, a ...any) int {
	_, _ = fmt.Fprintf(s.Stderr, "roomcity runtime: %s\n", fmt.Sprintf(format, a...))
	return 1
}

// readStdin returns all of r, or nothing for a terminal, which an operation
// run by hand has and would wait on.
func readStdin(r io.Reader) ([]byte, error) {
	if r == nil {
		return nil, nil
	}
	if f, ok := r.(*os.File); ok {
		if info, err := f.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
			return nil, nil
		}
	}
	b, err := io.ReadAll(r)
	if errors.Is(err, io.EOF) {
		err = nil
	}
	return b, err
}

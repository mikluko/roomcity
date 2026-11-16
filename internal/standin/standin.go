// Package standin is the gc of a session's pod: it sends its arguments, its
// input, its GC_* environment and its directory to the server, which runs the
// real gc in the city's directory, and ends with what comes back.
package standin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"connectrpc.com/connect"

	roomcityv1 "sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1"
	"sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1/roomcityv1connect"
	"sarak.sh/mikluko/roomcity/internal/sessionauth"
)

// The variables of the environment that name the session and its server.
const (
	EnvSession = "ROOMCITY_SESSION"
	EnvServer  = "ROOMCITY_SERVER"
)

// exitFailure is the code of a stand-in that could not get an answer. It is
// never 2, which gc's own commands use for "already done".
const exitFailure = 1

// Config names the session and the server the stand-in speaks for it to.
type Config struct {
	Session string
	// Token is the session's token, which every call carries.
	Token string
	// Server is the base URL of the server.
	Server string
	// HTTPClient is what the calls go through; nil is http.DefaultClient.
	HTTPClient connect.HTTPClient
}

// ConfigFromEnv reads the session, its token and the server from env, as
// os.Environ gives it.
func ConfigFromEnv(env []string) (Config, error) {
	var cfg Config
	for _, kv := range env {
		name, value, _ := strings.Cut(kv, "=")
		switch name {
		case EnvSession:
			cfg.Session = value
		case EnvServer:
			cfg.Server = value
		case sessionauth.EnvToken:
			cfg.Token = value
		}
	}
	switch {
	case cfg.Session == "":
		return Config{}, errors.New(EnvSession + " is not set")
	case cfg.Server == "":
		return Config{}, errors.New(EnvServer + " is not set")
	case cfg.Token == "":
		return Config{}, errors.New(sessionauth.EnvToken + " is not set")
	}
	return cfg, nil
}

// Streams is what the stand-in has of the process it runs in.
type Streams struct {
	// Stdin is read to its end before the call.
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	// Env is the environment, as os.Environ gives it; only GC_* is sent.
	Env []string
	// Dir is the working directory.
	Dir string
}

// Run asks the server to run gc with args and returns the exit code of that
// gc, after writing what it printed. A server that cannot be reached or
// refuses the call is exit code 1 with the reason on stderr.
func Run(ctx context.Context, cfg Config, args []string, s Streams) int {
	var stdin []byte
	if s.Stdin != nil {
		var err error
		if stdin, err = io.ReadAll(s.Stdin); err != nil {
			return fail(s.Stderr, fmt.Errorf("reading stdin: %w", err))
		}
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	client := roomcityv1connect.NewGcServiceClient(httpClient, cfg.Server, sessionauth.Client(cfg.Token))
	res, err := client.Run(ctx, connect.NewRequest(&roomcityv1.RunRequest{
		Session: cfg.Session,
		Args:    args,
		Stdin:   stdin,
		Env:     gcEnv(s.Env),
		Cwd:     s.Dir,
	}))
	if err != nil {
		return fail(s.Stderr, err)
	}
	_, _ = s.Stdout.Write(res.Msg.Stdout)
	_, _ = s.Stderr.Write(res.Msg.Stderr)
	return int(res.Msg.ExitCode)
}

func fail(stderr io.Writer, err error) int {
	_, _ = fmt.Fprintf(stderr, "gc: %v\n", err)
	return exitFailure
}

// gcEnv returns the GC_* variables of env.
func gcEnv(env []string) map[string]string {
	out := map[string]string{}
	for _, kv := range env {
		if name, value, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(name, "GC_") {
			out[name] = value
		}
	}
	return out
}

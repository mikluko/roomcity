// Package gchandler is the server's side of GcService: it runs the real gc,
// in the city's directory, for the stand-in gc of a session.
package gchandler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"connectrpc.com/connect"

	roomcityv1 "sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1"
	"sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1/roomcityv1connect"
	"sarak.sh/mikluko/roomcity/internal/sessionauth"
)

// Config is how the handler runs gc.
type Config struct {
	// CityDir is where gc runs, whatever directory the session ran the
	// stand-in in.
	CityDir string
	// GcPath is the gc to run; "" is gc on the server's PATH.
	GcPath string
	// Env is the environment gc starts from; nil is the server's own.
	Env []string
	// Authorize says whether token is the token of session. A handler with
	// none refuses every call.
	Authorize func(session, token string) bool
}

// Handler implements roomcityv1connect.GcServiceHandler.
type Handler struct {
	cfg Config
}

var _ roomcityv1connect.GcServiceHandler = (*Handler)(nil)

// New returns a handler that runs gc as cfg says.
func New(cfg Config) *Handler {
	if cfg.GcPath == "" {
		cfg.GcPath = "gc"
	}
	if cfg.Env == nil {
		cfg.Env = os.Environ()
	}
	return &Handler{cfg: cfg}
}

// Run runs gc once, for a session that proves itself by its token. A gc that ends with a non-zero code is an answer; a gc
// that does not start is an error. Only the session's GC_* variables are
// taken from the request, over the handler's own environment.
func (h *Handler) Run(ctx context.Context, req *connect.Request[roomcityv1.RunRequest]) (*connect.Response[roomcityv1.RunResponse], error) {
	msg := req.Msg
	if h.cfg.Authorize == nil || !h.cfg.Authorize(msg.Session, sessionauth.Token(req.Header())) {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("the session's token is missing or not its own"))
	}
	cmd := exec.CommandContext(ctx, h.cfg.GcPath, msg.Args...)
	cmd.Dir = h.cfg.CityDir
	cmd.Env = environ(h.cfg.Env, msg.Env)
	cmd.Stdin = bytes.NewReader(msg.Stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
	default:
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("gc: %w", err))
	}
	return connect.NewResponse(&roomcityv1.RunResponse{
		Stdout:   stdout.Bytes(),
		Stderr:   stderr.Bytes(),
		ExitCode: int32(cmd.ProcessState.ExitCode()),
	}), nil
}

// environ is base with the GC_* entries of session set over it.
func environ(base []string, session map[string]string) []string {
	env := make([]string, 0, len(base)+len(session))
	for _, kv := range base {
		name, _, _ := strings.Cut(kv, "=")
		if _, over := session[name]; over && strings.HasPrefix(name, "GC_") {
			continue
		}
		env = append(env, kv)
	}
	for name, value := range session {
		if strings.HasPrefix(name, "GC_") {
			env = append(env, name+"="+value)
		}
	}
	return env
}

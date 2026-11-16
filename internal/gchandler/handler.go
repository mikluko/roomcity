// Package gchandler is the server's side of GcService: it runs the real gc,
// in the city's directory, for the stand-in gc of a session.
package gchandler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"

	roomcityv1 "sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1"
	"sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1/roomcityv1connect"
	"sarak.sh/mikluko/roomcity/internal/runtimecmd"
	"sarak.sh/mikluko/roomcity/internal/session"
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
	// RuntimeURL is the address of the server's runtime listener, which
	// gc-runtime-room reaches for gc's own session operations. It is set in
	// gc's environment over anything else, "" leaves it unset.
	RuntimeURL string
	// Log gets one line per command; nil is slog.Default.
	Log *slog.Logger
	// Identity returns the variables by which gc knows who session is, as
	// the server keeps them of its start, and false for a session whose start
	// kept none; the request's own are then taken, as before. A nil Identity
	// takes the request's for every session.
	Identity func(ctx context.Context, session string) (map[string]string, bool, error)
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
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Env == nil {
		cfg.Env = os.Environ()
	}
	return &Handler{cfg: cfg}
}

// Run runs gc once, for a session that proves itself by its token. A call
// with no session or no token is refused before Authorize is asked. A gc that ends with a non-zero code is an answer; a gc
// that does not start is an error. The variables that say who is calling,
// session.IdentityEnv, are the server's record of the session's start, which
// a session does not decide; only for a session whose start kept none are
// they the request's. Every other variable is the server's, the call's own
// being its arguments and its input. Through gc a session can do what gc's
// command line does in the city.
func (h *Handler) Run(ctx context.Context, req *connect.Request[roomcityv1.RunRequest]) (*connect.Response[roomcityv1.RunResponse], error) {
	msg := req.Msg
	token := sessionauth.Token(req.Header())
	if msg.Session == "" || token == "" || h.cfg.Authorize == nil || !h.cfg.Authorize(msg.Session, token) {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("the session's token is missing or not its own"))
	}
	claimed, err := h.identity(ctx, msg.Session, msg.Env)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("the session's identity: %w", err))
	}
	res, err := h.run(ctx, msg.Session, claimed, msg.Args, msg.Stdin, false)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&roomcityv1.RunResponse{Stdout: res.Stdout, Stderr: res.Stderr, ExitCode: int32(res.ExitCode)}), nil
}

// Result is what a gc that ran answered.
type Result struct {
	Stdout, Stderr []byte
	ExitCode       int
}

// Exec runs gc for the server's own purposes as the session, which the
// caller has already authorized: the identity is the server's record of the
// session's start and nothing else. A gc that ends with a non-zero code is a
// Result; one that does not start is an error.
func (h *Handler) Exec(ctx context.Context, session string, args []string, stdin []byte) (Result, error) {
	return h.exec(ctx, session, args, stdin, false)
}

// ExecQuiet is Exec with the command's completion logged at DEBUG rather than
// INFO. A command that does not start is still logged at ERROR.
func (h *Handler) ExecQuiet(ctx context.Context, session string, args []string, stdin []byte) (Result, error) {
	return h.exec(ctx, session, args, stdin, true)
}

func (h *Handler) exec(ctx context.Context, session string, args []string, stdin []byte, quiet bool) (Result, error) {
	claimed, err := h.identity(ctx, session, nil)
	if err != nil {
		return Result{}, fmt.Errorf("the session's identity: %w", err)
	}
	return h.run(ctx, session, claimed, args, stdin, quiet)
}

func (h *Handler) run(ctx context.Context, session string, claimed map[string]string, args []string, stdin []byte, quiet bool) (Result, error) {
	cmd := exec.CommandContext(ctx, h.cfg.GcPath, args...)
	cmd.Dir = h.cfg.CityDir
	cmd.Env = environ(h.cfg.Env, claimed, h.cfg.RuntimeURL)
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	began := time.Now()
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
	default:
		h.cfg.Log.Error("gc did not start", "session", session, "command", name(args), "err", err)
		return Result{}, fmt.Errorf("gc: %w", err)
	}
	level := slog.LevelInfo
	if quiet {
		level = slog.LevelDebug
	}
	h.cfg.Log.Log(ctx, level, "gc ran", "session", session, "command", name(args), "exit", cmd.ProcessState.ExitCode(), "took", time.Since(began).Round(time.Millisecond))
	return Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: cmd.ProcessState.ExitCode()}, nil
}

// name is the command and, after it, the subcommand: the words gc's own
// command tree has. A later word may be what an agent typed, so a second word
// that is not a plain lowercase word is left out, and nothing past it is kept.
func name(args []string) string {
	var words []string
	for i, a := range args {
		if i == 2 || !plain(a) {
			break
		}
		words = append(words, a)
	}
	return strings.Join(words, " ")
}

func plain(s string) bool {
	if s == "" || len(s) > 20 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && r != '-' {
			return false
		}
	}
	return s[0] != '-'
}

// identity returns the variables gc is to take as the caller's: the server's
// record of the session's start where it has one, the request's otherwise.
func (h *Handler) identity(ctx context.Context, session string, requested map[string]string) (map[string]string, error) {
	if h.cfg.Identity == nil {
		return requested, nil
	}
	rec, ok, err := h.cfg.Identity(ctx, session)
	if err != nil || !ok {
		return requested, err
	}
	return rec, nil
}

// environ is base with the identity entries of the caller set over it, and the
// runtime's address set last.
func environ(base []string, caller map[string]string, runtimeURL string) []string {
	taken := func(name string) bool { return slices.Contains(session.IdentityEnv, name) }
	env := make([]string, 0, len(base)+len(caller)+1)
	for _, kv := range base {
		name, _, _ := strings.Cut(kv, "=")
		if _, over := caller[name]; over && taken(name) {
			continue
		}
		if name == runtimecmd.EnvURL && runtimeURL != "" {
			continue
		}
		env = append(env, kv)
	}
	for name, value := range caller {
		if taken(name) {
			env = append(env, name+"="+value)
		}
	}
	if runtimeURL != "" {
		env = append(env, runtimecmd.EnvURL+"="+runtimeURL)
	}
	return env
}

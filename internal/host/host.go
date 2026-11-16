// Package host is the program of a session's pod: it drives Claude Code
// through claudeacp, in its own process, as the ACP client, takes the
// session's instructions from the server and reports every ACP message to
// it.
package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/afrase/acp-go/steering"
	acp "github.com/afrase/acp-go/unstable"

	"sarak.sh/mikluko/claudeacp/claudeacp"

	"sarak.sh/mikluko/roomcity/internal/config"
	roomcityv1 "sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1"
	"sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1/roomcityv1connect"
	"sarak.sh/mikluko/roomcity/internal/sessionauth"
)

// Config is how a host runs.
type Config struct {
	// Session is the session's name, as gc gave it to the runtime.
	Session string
	// Token is the session's token for the server.
	Token string
	// Server is the base URL of the server.
	Server string
	// WorkDir is the agent's working directory; it is made when absent.
	WorkDir string
	// Claude is how claudeacp runs claude.
	Claude claudeacp.Config
	// HTTPClient carries the calls; nil is http.DefaultClient.
	HTTPClient connect.HTTPClient
	// Log receives what went wrong or changed; never a prompt or a token.
	// nil is silent.
	Log *slog.Logger
	// Retry is how long a failed call waits before it is made again; zero
	// is a second.
	Retry time.Duration
	// Now is the clock of the frames; nil is time.Now.
	Now func() time.Time
	// RoomTools are the tools of the room besides attach that the session has:
	// dispatch, wait, result, claim or done. The server names them.
	RoomTools []string
}

const (
	envSession = "ROOMCITY_SESSION"
	envServer  = "ROOMCITY_SERVER"
	envWorkDir = "ROOMCITY_WORKDIR"
	// envSettings names a settings file for claude, with the hooks gc gives
	// a terminal session; claude gets it as --settings. Optional.
	envSettings = "ROOMCITY_SETTINGS"
	// envRoomTools names, by commas, the work tools of the room the session
	// has besides attach. Optional.
	envRoomTools = "ROOMCITY_ROOM_TOOLS"
)

// ConfigFromEnv reads the session, its token, the server and the working
// directory from env, as os.Environ gives it, and the settings file of claude
// when env names one.
func ConfigFromEnv(env []string) (Config, error) {
	var cfg Config
	for _, kv := range env {
		name, value, _ := cut(kv)
		switch name {
		case envSession:
			cfg.Session = value
		case envServer:
			cfg.Server = value
		case envWorkDir:
			cfg.WorkDir = value
		case envSettings:
			if value != "" {
				cfg.Claude.ClaudeArgs = append(cfg.Claude.ClaudeArgs, "--settings", value)
			}
		case envRoomTools:
			for _, t := range strings.Split(value, ",") {
				if t = strings.TrimSpace(t); t != "" {
					cfg.RoomTools = append(cfg.RoomTools, t)
				}
			}
		case sessionauth.EnvToken:
			cfg.Token = value
		}
	}
	for _, need := range []struct{ name, value string }{
		{envSession, cfg.Session}, {sessionauth.EnvToken, cfg.Token}, {envServer, cfg.Server}, {envWorkDir, cfg.WorkDir},
	} {
		if need.value == "" {
			return Config{}, errors.New(need.name + " is not set")
		}
	}
	return cfg, nil
}

func cut(kv string) (name, value string, ok bool) {
	for i := range len(kv) {
		if kv[i] == '=' {
			return kv[:i], kv[i+1:], true
		}
	}
	return kv, "", false
}

// Run runs the host until the server tells it to stop, ctx ends, or claude
// does. A stop instruction and the end of ctx are not errors.
//
// Run takes an instruction as carried out when it has started it, and names
// that instruction when it attaches again after a lost connection. A Cancel
// also drops the owner's prompts that had not started: the owner's stop is for
// everything they wrote, and not for what gc sent.
func Run(ctx context.Context, cfg Config) error {
	h := newHost(cfg)
	return h.run(ctx)
}

type host struct {
	cfg    Config
	log    *slog.Logger
	rec    *recorder
	client roomcityv1connect.HostServiceClient

	mu      sync.Mutex
	lastSeq uint64
	sid     acp.SessionID
	conn    *acp.AgentConn
	prompts []queued
	turning bool
	// steering is whether the agent said, in its initialize answer, that it
	// takes a message into a running turn.
	steering bool
	// reports are what the server has not yet been told of a prompt, in the
	// order they happened, and wake says there is one.
	reports []handledReport
	wake    chan struct{}
	pending map[string]chan string // permission requests by their id
	endHost context.CancelFunc
	// room serves the agent the room's tools; nil when the agent takes no
	// http MCP server.
	room *room
	// own tells the permission requests that are for the room's own tools.
	own *ownRequests
	// fatalErr is what ended the host when it was not a stop.
	fatalErr error
}

func newHost(cfg Config) *host {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Retry <= 0 {
		cfg.Retry = time.Second
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &host{
		cfg:     cfg,
		log:     log,
		rec:     newRecorder(cfg.Now),
		client:  roomcityv1connect.NewHostServiceClient(httpClient, cfg.Server, sessionauth.Client(cfg.Token)),
		pending: map[string]chan string{},
		wake:    make(chan struct{}, 1),
		own:     newOwnRequests(roomToolNames(cfg.RoomTools)),
	}
}

func (h *host) run(ctx context.Context) error {
	if err := os.MkdirAll(h.cfg.WorkDir, 0o755); err != nil {
		return fmt.Errorf("host: working directory: %w", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	h.endHost = cancel
	defer func() {
		if h.room != nil {
			h.room.stop()
		}
	}()

	if err := h.resume(ctx); err != nil {
		return err
	}

	manifest, err := h.stage(ctx)
	if err != nil {
		return err
	}
	if err := h.runCommands(ctx, manifest.PreStart, true); err != nil {
		return err
	}
	ag, err := h.startAgent(ctx, manifest.Command)
	if err != nil {
		return err
	}
	// The host's client of the agent speaks to it over its streams, each
	// tapped where its messages pass.
	conn := acp.NewAgentConn(
		newTapReader(ag.r, h.rec, roomcityv1.FrameDirection_FRAME_DIRECTION_FROM_AGENT, h.own.see),
		newTapWriter(ag.w, h.rec, roomcityv1.FrameDirection_FRAME_DIRECTION_TO_AGENT),
		func(*acp.AgentConn) acp.Client { return &agentClient{h: h} },
	)
	h.conn = conn

	var wg sync.WaitGroup
	wg.Go(func() { h.report(ctx) })

	err = h.session(ctx)
	if err != nil {
		if why := ag.whyEnded(); why != "" {
			err = fmt.Errorf("%w (%s)", err, why)
			h.log.Error("the agent ended while the session was starting", "why", why)
		}
	}
	if err == nil {
		_ = h.runCommands(ctx, manifest.SessionSetup, false)
		h.runScript(ctx, manifest.SessionSetupScript)
		_ = h.runCommands(ctx, manifest.SessionLive, false)
		wg.Go(func() { h.attach(ctx) })
		select {
		case <-ctx.Done():
		case <-conn.Done():
			if ctx.Err() == nil {
				err = fmt.Errorf("host: the agent's connection ended: %w", errors.Unwrap(conn.Err()))
				if why := ag.whyEnded(); why != "" {
					err = fmt.Errorf("%w (%s)", err, why)
					h.log.Error("the agent ended", "why", why)
				}
			}
		}
	}
	ended := ctx.Err() != nil
	cancel()
	_ = ag.w.Close()
	<-ag.ended
	sErr := ag.err
	wg.Wait()
	h.flush()
	h.mu.Lock()
	fatal := h.fatalErr
	h.mu.Unlock()
	switch {
	case fatal != nil:
		return fatal
	case err != nil:
		return err
	case !ended && sErr != nil:
		return sErr
	}
	return nil
}

// fatal ends the host because of err, which no retry will mend.
func (h *host) fatal(err error) {
	h.mu.Lock()
	if h.fatalErr == nil {
		h.fatalErr = err
	}
	h.mu.Unlock()
	h.endHost()
}

// session starts the agent's session: initialize, then session/new.
func (h *host) session(ctx context.Context) error {
	hello, err := h.conn.Initialize(ctx, &acp.InitializeRequest{ProtocolVersion: acp.LatestProtocolVersion, ClientCapabilities: &acp.ClientCapabilities{}})
	if err != nil {
		return fmt.Errorf("host: initialize: %w%s", err, errorData(err))
	}
	h.mu.Lock()
	h.steering = steering.Supported(hello.Meta)
	h.mu.Unlock()
	servers, err := h.roomServers(hello)
	if err != nil {
		return err
	}
	res, err := h.conn.NewSession(ctx, &acp.NewSessionRequest{Cwd: h.cfg.WorkDir, MCPServers: servers})
	if err != nil {
		return fmt.Errorf("host: new session: %w%s", err, errorData(err))
	}
	h.mu.Lock()
	h.sid = res.SessionID
	h.mu.Unlock()
	return nil
}

// roomServers starts the room's tools and returns the MCP servers session/new
// gives the agent: the room's, when the agent says it takes an http one.
func (h *host) roomServers(hello *acp.InitializeResponse) ([]acp.MCPServer, error) {
	caps := hello.AgentCapabilities
	if caps == nil || caps.MCPCapabilities == nil || !caps.MCPCapabilities.HTTP {
		h.log.Info("the agent takes no http MCP server: the room's tools are not given to it")
		return []acp.MCPServer{}, nil
	}
	for _, t := range h.cfg.RoomTools {
		if !slices.Contains(config.RoomTools, t) {
			h.log.Warn("a tool the server names is not one of the room's, and is left out", "tool", t)
		}
	}
	r, err := startRoom(h.cfg.WorkDir, h.cfg.Session, h.client, roomToolNames(h.cfg.RoomTools))
	if err != nil {
		return nil, fmt.Errorf("host: the room's tools: %w", err)
	}
	h.mu.Lock()
	h.room = r
	h.mu.Unlock()
	name, address, header := r.entry()
	return []acp.MCPServer{{HTTP: &acp.MCPServerHTTP{Name: name, URL: address, Headers: []acp.HTTPHeader{{Name: "Authorization", Value: header}}}}}, nil
}

// wait waits for d or ctx's end, and reports whether ctx is still live.
func wait(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// errorData returns the data an ACP error carries, which its message leaves
// out, as " (data: ...)", or "" when there is none.
func errorData(err error) string {
	var e *acp.Error
	if !errors.As(err, &e) || e.Data == nil {
		return ""
	}
	b, merr := json.Marshal(e.Data)
	if merr != nil || len(b) == 0 || string(b) == "null" {
		return ""
	}
	if len(b) > 500 {
		b = append(b[:500:500], "..."...)
	}
	return " (data: " + string(b) + ")"
}

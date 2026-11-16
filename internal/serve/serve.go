// Package serve is "roomcity server": it opens the registry, mounts the
// services on the server's three listeners and serves them until it is told
// to stop.
package serve

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"sarak.sh/mikluko/roomcity/internal/config"
	"sarak.sh/mikluko/roomcity/internal/gchandler"
	"sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1/roomcityv1connect"
	"sarak.sh/mikluko/roomcity/internal/operate"
	"sarak.sh/mikluko/roomcity/internal/procs"
	"sarak.sh/mikluko/roomcity/internal/server"
	"sarak.sh/mikluko/roomcity/internal/session"
	"sarak.sh/mikluko/roomcity/internal/transcripts"
)

// DatabaseFile is the registry's file in the configuration's data directory.
const DatabaseFile = "roomcity.db"

// ProcessLogDir is the directory of the data directory that holds the output
// of the processes the server runs.
const ProcessLogDir = "processes"

// shutdownGrace is how long the listeners get to finish a call once the
// server is told to stop.
const shutdownGrace = 10 * time.Second

// Listeners are the three the server serves on.
type Listeners struct {
	// Network is what the session pods and the homeserver reach, Runtime
	// is gc's supervisor's alone and Page is the authenticating proxy's.
	Network, Runtime, Page net.Listener
}

// Listen opens the listeners the configuration names.
func Listen(cfg config.Config) (Listeners, error) {
	var l Listeners
	var err error
	for _, p := range []struct {
		into *net.Listener
		addr string
	}{{&l.Network, cfg.Server.Listen}, {&l.Runtime, cfg.Server.RuntimeListen}, {&l.Page, cfg.Server.PageListen}} {
		if *p.into, err = net.Listen("tcp", p.addr); err != nil {
			l.Close()
			return Listeners{}, err
		}
	}
	return l, nil
}

// Close closes the listeners that are open.
func (l Listeners) Close() {
	for _, c := range []net.Listener{l.Network, l.Runtime, l.Page} {
		if c != nil {
			_ = c.Close()
		}
	}
}

// Options are what Run serves with.
type Options struct {
	Config    config.Config
	Listeners Listeners
	// Jobs is the Kubernetes side of the sessions.
	Jobs operate.Jobs
	// Files is where a file a session sends goes to its room; nil is a server
	// with no chat side to send to.
	Files server.Files
	Log   *slog.Logger
}

// resetter asks gc to reset a session: `gc session reset <id of its bead>`, run
// in the city's directory as gc's own command through the server, as the stand-in
// gc's calls are. gc stops the session's runtime and starts it again, which
// the runtime sees as a stop and a start of the same name.
func resetter(cfg config.Config) func(ctx context.Context, gcID string) error {
	return func(ctx context.Context, gcID string) error {
		ctx, cancel := context.WithTimeout(ctx, resetTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, cfg.Server.Gc, "session", "reset", gcID)
		cmd.Dir = cfg.Server.CityDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("gc session reset %s: %w: %s", gcID, err, strings.TrimSpace(string(out)))
		}
		return nil
	}
}

// resetTimeout is how long gc gets to take a reset: it writes the request on
// the session's bead and wakes the controller, and the stop and the start
// come afterwards, on their own.
const resetTimeout = 30 * time.Second

// purgeTranscripts removes the transcripts of starts that ended more than
// term ago, each start by its own end, now and then, until ctx ends.
func purgeTranscripts(ctx context.Context, reg *session.Registry, term, every time.Duration, log *slog.Logger) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		n, err := reg.Purge(ctx, time.Now().Add(-term))
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			log.Error("removing the transcripts past their term", "err", err)
		case n > 0:
			log.Info("removed the transcripts past their term", "starts", n)
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			return
		}
	}
}

// Run serves until ctx ends or a listener fails, and closes the registry
// before it returns.
func Run(ctx context.Context, o Options) error {
	cfg := o.Config
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	if err := os.MkdirAll(cfg.Server.Data, 0o750); err != nil {
		return err
	}
	reg, err := session.Open(ctx, filepath.Join(cfg.Server.Data, DatabaseFile))
	if err != nil {
		return err
	}
	if backup := reg.BackupPath(); backup != "" {
		log.Info("migrated the database; the copy of it as it was, which a server of the earlier version runs on, is kept until an operator removes it", "copy", backup)
	}
	defer func() { _ = reg.Close() }()
	reg.SetResetter(resetter(cfg))

	s := server.New()
	s.Handle(roomcityv1connect.NewHostServiceHandler(&server.Host{Registry: reg, Files: o.Files, Log: log}))
	s.Handle(roomcityv1connect.NewGcServiceHandler(gchandler.New(gchandler.Config{
		CityDir:    cfg.Server.CityDir,
		GcPath:     cfg.Server.Gc,
		Authorize:  reg.Authorize,
		Identity:   reg.Identity,
		RuntimeURL: "http://" + o.Listeners.Runtime.Addr().String(),
		Log:        log,
	})))
	s.HandleRuntime(roomcityv1connect.NewRuntimeServiceHandler(operate.New(reg, o.Jobs, operate.Config{
		ServerURL:    cfg.Server.URL,
		AlwaysOn:     cfg.AlwaysOn,
		StartTimeout: cfg.Server.StartTimeout.Duration(),
		StopGrace:    cfg.Server.StopGrace.Duration(),
		IsProcess:    cfg.IsProcess,
		Processes:    &procs.Manager{LogDir: filepath.Join(cfg.Server.Data, ProcessLogDir)},
		Log:          log,
	})))

	side, err := newChatSide(cfg, reg, log)
	if err != nil {
		return err
	}
	if side != nil {
		s.Handle(appServicePath, side.Handler)
	}
	s.HandlePage(transcripts.Prefix, (&transcripts.Page{Source: reg, City: side.city(cfg)}).Handler())

	g, ctx := errgroup.WithContext(ctx)
	if side != nil {
		g.Go(func() error { return side.Service.Run(ctx) })
	}
	g.Go(func() error {
		purgeTranscripts(ctx, reg, cfg.Server.TranscriptTerm.Duration(), cfg.Server.PurgeEvery.Duration(), log)
		return nil
	})
	for _, l := range []struct {
		name string
		on   net.Listener
		h    http.Handler
	}{
		{"network", o.Listeners.Network, server.LimitSend(s.Handler(), reg, server.DefaultMaxFile)},
		{"runtime", o.Listeners.Runtime, s.RuntimeHandler()},
		{"page", o.Listeners.Page, s.PageHandler()},
	} {
		srv := &http.Server{Handler: l.h, ReadHeaderTimeout: 10 * time.Second}
		g.Go(func() error {
			log.Info("listening", "listener", l.name, "addr", l.on.Addr().String())
			if err := srv.Serve(l.on); !errors.Is(err, http.ErrServerClosed) {
				return fmt.Errorf("%s listener: %w", l.name, err)
			}
			return nil
		})
		g.Go(func() error {
			<-ctx.Done()
			down, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
			defer cancel()
			if err := srv.Shutdown(down); err != nil {
				_ = srv.Close()
			}
			return nil
		})
	}
	return g.Wait()
}

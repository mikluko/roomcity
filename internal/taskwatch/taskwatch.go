// Package taskwatch ends the task sessions that cannot finish their task: one
// that has run past its agent's task timeout, and one whose model fails its
// turns again and again. Nothing else ends such a session: gc's own nudges to
// claim give up, the task stays open, and demand keeps the session.
package taskwatch

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"sarak.sh/mikluko/roomcity/internal/session"
)

// Ender ends a session that cannot finish its task: the task is closed as
// abandoned, with reason, and the session is ended.
type Ender interface {
	Fail(ctx context.Context, session, reason string) error
}

// Sessions is the registry as the watch reads it.
type Sessions interface {
	Get(ctx context.Context, name string) (session.Info, error)
	List(ctx context.Context) ([]session.Info, error)
	Starts(ctx context.Context, name string) ([]session.Start, error)
}

// Watch ends task sessions as Timeout and FailedTurns say.
type Watch struct {
	Sessions Sessions
	Ender    Ender
	// Timeout is how long an agent's task session may run; zero is for as long
	// as it takes.
	Timeout func(agent string) time.Duration
	// FailedTurns is how many turns in a row that end in an error end the
	// agent's task session; zero is none.
	FailedTurns func(agent string) int
	// Every is how often the clock is read; a minute when zero.
	Every time.Duration
	Now   func() time.Time
	Log   *slog.Logger

	mu    sync.Mutex
	turns map[string]*turns
	ended map[string]bool
}

// turns is what the watch has seen of a session's turns.
type turns struct {
	prompts map[string]bool
	failed  int
	last    string
}

func (w *Watch) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Watch) log() *slog.Logger {
	if w.Log != nil {
		return w.Log
	}
	return slog.Default()
}

// message is the part of a JSON-RPC message the watch reads.
type message struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Observe is given the frames of a session as the host reports them. A
// response with an error to a session/prompt is a failed turn; a response
// without one ends the run of them.
func (w *Watch) Observe(ctx context.Context, name string, frames []session.HostFrame) {
	w.mu.Lock()
	t := w.turns[name]
	if t == nil {
		if w.turns == nil {
			w.turns = map[string]*turns{}
		}
		t = &turns{prompts: map[string]bool{}}
		w.turns[name] = t
	}
	for _, f := range frames {
		var m message
		if json.Unmarshal(f.Message, &m) != nil || len(m.ID) == 0 {
			continue
		}
		id := string(m.ID)
		switch {
		case f.Direction == session.ToAgent && m.Method == "session/prompt":
			t.prompts[id] = true
		case f.Direction == session.FromAgent && m.Method == "" && t.prompts[id]:
			delete(t.prompts, id)
			if m.Error != nil {
				t.failed++
				t.last = m.Error.Message
			} else {
				t.failed = 0
			}
		}
	}
	failed, last := t.failed, t.last
	w.mu.Unlock()
	if failed == 0 {
		return
	}
	info, err := w.Sessions.Get(ctx, name)
	if err != nil || info.Kind != session.Task || w.FailedTurns == nil {
		return
	}
	limit := w.FailedTurns(info.Agent)
	if limit == 0 || failed < limit {
		return
	}
	reason := fmt.Sprintf("ended without a result: its model failed %d turns in a row, the last with: %s", failed, last)
	go w.end(context.WithoutCancel(ctx), name, reason)
}

// Run reads the clock every Every until ctx ends.
func (w *Watch) Run(ctx context.Context) {
	every := w.Every
	if every <= 0 {
		every = time.Minute
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			w.Check(ctx)
		}
	}
}

// Check ends the running task sessions that are past their agent's timeout.
func (w *Watch) Check(ctx context.Context) {
	if w.Timeout == nil {
		return
	}
	all, err := w.Sessions.List(ctx)
	if err != nil {
		w.log().Error("listing the sessions for their timeouts", "err", err)
		return
	}
	for _, info := range all {
		limit := w.Timeout(info.Agent)
		if limit <= 0 || info.Kind != session.Task || info.State != session.Running {
			continue
		}
		starts, err := w.Sessions.Starts(ctx, info.Name)
		if err != nil || len(starts) == 0 {
			continue
		}
		began := starts[len(starts)-1].Began
		if w.now().Sub(began) > limit {
			w.end(ctx, info.Name, fmt.Sprintf("ended without a result: timed out after %s", limit))
		}
	}
}

// end ends the session once, for as long as the watch is up.
func (w *Watch) end(ctx context.Context, name, reason string) {
	w.mu.Lock()
	if w.ended == nil {
		w.ended = map[string]bool{}
	}
	if w.ended[name] {
		w.mu.Unlock()
		return
	}
	w.ended[name] = true
	w.mu.Unlock()
	if err := w.Ender.Fail(ctx, name, reason); err != nil {
		w.mu.Lock()
		delete(w.ended, name)
		w.mu.Unlock()
		w.log().Error("ending a task session that cannot finish", "session", name, "reason", reason, "err", err)
		return
	}
	w.log().Info("ended a task session that cannot finish", "session", name, "reason", reason)
}

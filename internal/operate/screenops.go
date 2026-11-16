package operate

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	roomcityv1 "sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1"
	"sarak.sh/mikluko/roomcity/internal/screen"
	"sarak.sh/mikluko/roomcity/internal/session"
)

// readyPromptMark is the mark under which gc keeps a session's ready prompt:
// it stores it with set-meta after a start and reads it back to scan the
// screen for it.
const readyPromptMark = "GC_READY_PROMPT_PREFIX"

// capabilities are what the runtime declares to gc in its handshake. A
// terminal-less runtime drives its session through the five dedicated
// operations of carrier.ops (nudge, peek, send-keys, interrupt and
// clear-scrollback) and has an idle boundary, which gc reads from the screen
// peek returns; it reports when the stream last moved, and answers that
// nobody is attached.
var capabilities = []string{"carrier.ops", "idle-boundary", "report-activity", "report-attachment"}

// protocol answers gc's handshake.
func protocol() result {
	b, err := json.Marshal(struct {
		Version      int      `json:"version"`
		Capabilities []string `json:"capabilities"`
	}{0, capabilities})
	if err != nil {
		return failf("protocol: %v", err)
	}
	return ok(string(b))
}

// peek returns the last lines of the session's screen: what the room shows
// as text, then the ready prompt, or the busy line while a turn runs.
func (o *Operator) peek(ctx context.Context, name, lines string) result {
	n := 0
	if lines != "" {
		v, err := strconv.Atoi(lines)
		if err != nil {
			return failf("peek %s: %q is not a number of lines", name, lines)
		}
		n = v
	}
	if o.isProcess(name) {
		return ok(strings.Join(o.cfg.Processes.Tail(name, n), "\n"))
	}
	if _, err := o.reg.Get(ctx, name); err != nil {
		return failf("peek %s: %v", name, err)
	}
	act, err := o.reg.Activity(ctx, name)
	if err != nil {
		return failf("peek %s: %v", name, err)
	}
	events, err := o.reg.Tail(ctx, name, 2000)
	if err != nil {
		return failf("peek %s: %v", name, err)
	}
	prompt, err := o.reg.Mark(ctx, name, readyPromptMark)
	if err != nil {
		return failf("peek %s: %v", name, err)
	}
	return ok(strings.Join(screen.Render(o.startedCommand(ctx, name), events, n, act.Turn, prompt), "\n"))
}

// startedCommand returns the command line the session's agent was started
// with, as the start staged it, or "" when there is none.
func (o *Operator) startedCommand(ctx context.Context, name string) string {
	raw, _, err := o.reg.Bundle(ctx, name)
	if err != nil {
		return ""
	}
	var m roomcityv1.StageManifest
	if proto.Unmarshal(raw, &m) != nil {
		return ""
	}
	return strings.Join(m.Command, " ")
}

// lastActivity returns the time of the session's latest frame as RFC 3339,
// or nothing for a session with none.
func (o *Operator) lastActivity(ctx context.Context, name string) result {
	act, err := o.reg.Activity(ctx, name)
	switch {
	case errors.Is(err, session.ErrNotFound):
		return ok("")
	case err != nil:
		return failf("get-last-activity %s: %v", name, err)
	case act.Last.IsZero():
		return ok("")
	}
	return ok(act.Last.UTC().Format(time.RFC3339))
}

// interrupt ends the running turn and drops nothing: what waits runs after
// it, the owner's first, which the owner's !stop does not do. gc asks it of a
// session that may not exist, and that is no failure.
func (o *Operator) interrupt(ctx context.Context, name string) result {
	err := o.reg.Interrupt(ctx, name)
	if err != nil && !errors.Is(err, session.ErrNotFound) && !errors.Is(err, session.ErrNotRunning) {
		return failf("interrupt %s: %v", name, err)
	}
	return ok("")
}

// sendKeys answers gc's keystrokes. A session has no keyboard: Escape and
// Ctrl-C, which gc sends to stop a turn, end the turn, and the rest are
// dropped.
func (o *Operator) sendKeys(ctx context.Context, name string, keys []string) result {
	for _, k := range keys {
		switch k {
		case "Escape", "C-c":
			return o.interrupt(ctx, name)
		}
	}
	return ok("")
}

// isAttached answers false: nobody attaches to a session, the room is for
// looking.
func isAttached() result { return ok("false") }

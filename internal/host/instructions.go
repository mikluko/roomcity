package host

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"connectrpc.com/connect"
	acp "github.com/afrase/acp-go/unstable"

	roomcityv1 "sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1"
)

// attach takes the server's instructions until ctx ends or one says stop. A
// connection that is lost is opened again, naming the last instruction
// taken.
func (h *host) attach(ctx context.Context) {
	for ctx.Err() == nil {
		h.mu.Lock()
		last := h.lastSeq
		h.mu.Unlock()
		stream, err := h.client.Attach(ctx, connect.NewRequest(&roomcityv1.AttachRequest{Session: h.cfg.Session, LastSeq: last}))
		if err == nil {
			for stream.Receive() {
				h.take(ctx, stream.Msg())
			}
			err = stream.Err()
			_ = stream.Close()
		}
		if ctx.Err() != nil {
			return
		}
		if connect.CodeOf(err) == connect.CodeUnauthenticated {
			h.fatal(fmt.Errorf("host: the server refused the session's token: %w", err))
			return
		}
		h.log.Warn("attaching to the server failed; trying again", "err", err)
		if !wait(ctx, h.cfg.Retry) {
			return
		}
	}
}

// take carries out one instruction, unless an earlier attachment has
// already delivered it.
func (h *host) take(ctx context.Context, in *roomcityv1.AttachResponse) {
	h.mu.Lock()
	if in.Seq <= h.lastSeq {
		h.mu.Unlock()
		return
	}
	h.lastSeq = in.Seq
	h.mu.Unlock()

	switch i := in.Instruction.(type) {
	case *roomcityv1.AttachResponse_Prompt:
		h.prompt(ctx, i.Prompt.GetText(), i.Prompt.GetSource())
	case *roomcityv1.AttachResponse_Cancel:
		h.cancel(ctx)
	case *roomcityv1.AttachResponse_PermissionAnswer:
		h.answer(i.PermissionAnswer)
	case *roomcityv1.AttachResponse_Stop:
		h.endHost()
	default:
		h.log.Warn("an instruction of a kind the host does not know", "seq", in.Seq)
	}
}

// queued is a prompt waiting for its turn.
type queued struct {
	text   string
	source roomcityv1.PromptSource
}

// prompt queues text as a turn; the turns run one at a time. A prompt the
// owner wrote goes ahead of those gc sent that are still waiting, behind the
// owner's own, in the order they came.
func (h *host) prompt(ctx context.Context, text string, source roomcityv1.PromptSource) {
	h.mu.Lock()
	q := queued{text, source}
	at := len(h.prompts)
	if source == roomcityv1.PromptSource_PROMPT_SOURCE_OWNER {
		for at > 0 && h.prompts[at-1].source != roomcityv1.PromptSource_PROMPT_SOURCE_OWNER {
			at--
		}
	}
	h.prompts = slices.Insert(h.prompts, at, q)
	start := !h.turning
	h.turning = true
	h.mu.Unlock()
	if start {
		go h.turns(ctx)
	}
}

// turns runs the queued prompts until none is left.
func (h *host) turns(ctx context.Context) {
	for {
		h.mu.Lock()
		if len(h.prompts) == 0 || ctx.Err() != nil {
			h.turning = false
			h.mu.Unlock()
			return
		}
		text := h.prompts[0].text
		h.prompts = h.prompts[1:]
		sid := h.sid
		h.mu.Unlock()

		_, err := h.conn.Prompt(ctx, &acp.PromptRequest{
			SessionID: sid,
			Prompt:    []acp.ContentBlock{{Text: &acp.TextContent{Text: text}}},
		})
		if err != nil && ctx.Err() == nil {
			h.log.Warn("a turn failed", "err", err)
		}
	}
}

// cancel ends the running turn, drops the prompts waiting behind it and
// answers every open permission request as cancelled, as ACP asks of the
// client that cancels.
func (h *host) cancel(ctx context.Context) {
	h.mu.Lock()
	h.prompts = nil
	sid := h.sid
	for _, ch := range h.pending {
		select {
		case ch <- "":
		default:
		}
	}
	h.mu.Unlock()
	if err := h.conn.Cancel(ctx, &acp.CancelNotification{SessionID: sid}); err != nil && !errors.Is(err, context.Canceled) {
		h.log.Warn("cancelling the turn failed", "err", err)
	}
}

// answer hands a permission request's answer to the call waiting for it.
func (h *host) answer(a *roomcityv1.PermissionAnswer) {
	h.mu.Lock()
	ch, ok := h.pending[a.RequestId]
	h.mu.Unlock()
	if !ok {
		h.log.Warn("an answer to a permission request nobody waits for", "request", a.RequestId)
		return
	}
	select {
	case ch <- a.OptionId:
	default:
	}
}

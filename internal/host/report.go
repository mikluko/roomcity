package host

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect"

	roomcityv1 "sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1"
)

// resume asks the server which frame it holds last, with a Report that
// carries none, and numbers the host's frames after it. It is made again
// until it succeeds or ctx ends.
func (h *host) resume(ctx context.Context) error {
	for {
		res, err := h.client.Report(ctx, connect.NewRequest(&roomcityv1.ReportRequest{Session: h.cfg.Session}))
		if err == nil {
			h.rec.continueAfter(res.Msg.LastSeq)
			return nil
		}
		if connect.CodeOf(err) == connect.CodeUnauthenticated {
			return fmt.Errorf("host: the server refused the session's token: %w", err)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		h.log.Warn("asking the server for its last frame failed; trying again", "err", err)
		if !wait(ctx, h.cfg.Retry) {
			return ctx.Err()
		}
	}
}

// batch is the most frames one Report carries.
const batch = 200

// report hands the server the frames it has not acknowledged, until ctx
// ends. A call that fails is made again, with the same frames: the server
// takes a frame by its seq once.
func (h *host) report(ctx context.Context) {
	for {
		if frames := h.rec.unacknowledged(batch); len(frames) > 0 {
			if err := h.send(ctx, frames); err != nil {
				if connect.CodeOf(err) == connect.CodeUnauthenticated {
					h.fatal(fmt.Errorf("host: the server refused the session's token: %w", err))
					return
				}
				h.log.Warn("reporting frames failed", "err", err)
				if !wait(ctx, h.cfg.Retry) {
					return
				}
			}
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-h.rec.wake:
		}
	}
}

func (h *host) send(ctx context.Context, frames []*roomcityv1.Frame) error {
	res, err := h.client.Report(ctx, connect.NewRequest(&roomcityv1.ReportRequest{Session: h.cfg.Session, Frames: frames}))
	if err != nil {
		return err
	}
	h.rec.acknowledge(res.Msg.LastSeq)
	return nil
}

// flush makes one last attempt at the frames still unacknowledged, when the
// host ends.
func (h *host) flush() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		frames := h.rec.unacknowledged(batch)
		if len(frames) == 0 || h.send(ctx, frames) != nil {
			return
		}
	}
}

// handledReport is what the server is to be told of a prompt.
type handledReport struct {
	seq      uint64
	handling roomcityv1.Handling
}

// handled queues the report of what the host did with a prompt. It is sent in
// the background, in the order the reports were made, and again after a
// failure or a lost connection: the server takes a report twice as once.
func (h *host) handled(seq uint64, handling roomcityv1.Handling) {
	h.mu.Lock()
	h.reports = append(h.reports, handledReport{seq, handling})
	h.mu.Unlock()
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

// reportHandled sends the reports until ctx ends. A report the server cannot
// place, because the instruction is not its own, is dropped.
func (h *host) reportHandled(ctx context.Context) {
	for {
		h.mu.Lock()
		var next *handledReport
		if len(h.reports) > 0 {
			next = &h.reports[0]
		}
		h.mu.Unlock()
		if next == nil {
			select {
			case <-h.wake:
			case <-ctx.Done():
				return
			}
			continue
		}
		_, err := h.client.Handled(ctx, connect.NewRequest(&roomcityv1.HandledRequest{
			Session: h.cfg.Session, InstructionSeq: next.seq, Handling: next.handling,
		}))
		if err != nil && ctx.Err() == nil && connect.CodeOf(err) != connect.CodeNotFound {
			h.log.Warn("telling the server what became of a prompt failed; trying again", "err", err)
			if !wait(ctx, h.cfg.Retry) {
				return
			}
			continue
		}
		h.mu.Lock()
		h.reports = h.reports[1:]
		h.mu.Unlock()
	}
}

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

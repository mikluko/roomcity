package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrFrameGap is returned by Report for a frame that is not the next host
// number.
var ErrFrameGap = errors.New("session: host frame out of order")

// rpc is what Report reads of a JSON-RPC message.
type rpc struct {
	Method string
	// ID is the request id as JSON, "" for a notification.
	ID string
	// OK is true for a response that carries a result.
	OK bool
}

func parseRPC(b []byte) rpc {
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if json.Unmarshal(b, &m) != nil {
		return rpc{}
	}
	id := string(m.ID)
	if id == "null" {
		id = ""
	}
	return rpc{Method: m.Method, ID: id, OK: len(m.Result) > 0 && len(m.Error) == 0}
}

// InstructionKind says what an Instruction tells the session host to do.
type InstructionKind int

const (
	InstructionPrompt InstructionKind = iota + 1
	InstructionCancel
	InstructionPermissionAnswer
	InstructionStop
)

// Instruction is one entry of a session's queue, numbered from 1 in the
// order it was queued. The numbering restarts when the session is started
// afresh.
type Instruction struct {
	Seq  uint64
	Kind InstructionKind
	// Source and Text are set for a prompt.
	Source Source
	Text   string
	// RequestID and OptionID are set for a permission answer.
	RequestID string
	OptionID  string
}

// HostFrame is a frame as the session host reports it, with the host's own
// numbering, from 1 for each start of the session.
type HostFrame struct {
	Seq uint64
	// Time is when the frame passed the host; zero means the server stores
	// it under its own clock.
	Time      time.Time
	Direction Direction
	Message   []byte
}

// enqueue stores in under the session's next instruction number. A session
// that has ended takes none.
func (r *Registry) enqueue(tx *sql.Tx, name string, in Instruction, eventSeq uint64) error {
	var state State
	if err := tx.QueryRow(`SELECT state FROM sessions WHERE name = ?`, name).Scan(&state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if state == Ended {
		return ErrNotRunning
	}
	var seq uint64
	if err := tx.QueryRow(`UPDATE sessions SET inst_seq = inst_seq + 1 WHERE name = ? RETURNING inst_seq`, name).Scan(&seq); err != nil {
		return err
	}
	_, err := tx.Exec(`
		INSERT INTO instructions (session, seq, kind, source, text, request_id, option_id, event_seq)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		name, seq, in.Kind, in.Source, in.Text, in.RequestID, in.OptionID, eventSeq)
	return err
}

func (r *Registry) queuePrompt(ctx context.Context, name string, src Source, text string) (uint64, error) {
	var seq uint64
	err := r.tx(ctx, func(tx *sql.Tx) error {
		var one int
		if err := tx.QueryRow(`SELECT 1 FROM sessions WHERE name = ?`, name).Scan(&one); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if err := r.enqueue(tx, name, Instruction{Kind: InstructionPrompt, Source: src, Text: text}, 0); err != nil {
			return err
		}
		var err error
		seq, err = r.appendEvent(tx, name, Prompt{Source: src, Text: text, State: Stored})
		if err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE instructions SET event_seq = ? WHERE session = ? AND seq = (SELECT inst_seq FROM sessions WHERE name = ?)`, seq, name, name)
		return err
	})
	if err == nil {
		r.changed()
	}
	return seq, err
}

// Prompt implements Service. The prompt's images are not carried yet.
func (r *Registry) Prompt(ctx context.Context, name string, in PromptInput) (uint64, error) {
	return r.queuePrompt(ctx, name, FromOwner, in.Text)
}

// Nudge queues gc's nudge as a prompt marked as gc's and returns the Seq of
// its event.
func (r *Registry) Nudge(ctx context.Context, name, text string) (uint64, error) {
	return r.queuePrompt(ctx, name, FromGC, text)
}

// Cancel implements Service: it drops the prompts not yet sent to the host
// and queues the cancellation, which the host carries out as ACP's
// session/cancel, withdrawing an open permission request with it.
func (r *Registry) Cancel(ctx context.Context, name string) error {
	return r.queueControl(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM instructions WHERE session = ? AND kind = ? AND sent = 0`, name, InstructionPrompt); err != nil {
			return err
		}
		return r.enqueue(tx, name, Instruction{Kind: InstructionCancel}, 0)
	})
}

// AnswerPermission implements Service.
func (r *Registry) AnswerPermission(ctx context.Context, name, requestID, optionID string) error {
	return r.queueControl(ctx, func(tx *sql.Tx) error {
		return r.enqueue(tx, name, Instruction{Kind: InstructionPermissionAnswer, RequestID: requestID, OptionID: optionID}, 0)
	})
}

// Stop queues the instruction that ends the agent and the host.
func (r *Registry) Stop(ctx context.Context, name string) error {
	return r.queueControl(ctx, func(tx *sql.Tx) error {
		return r.enqueue(tx, name, Instruction{Kind: InstructionStop}, 0)
	})
}

func (r *Registry) queueControl(ctx context.Context, f func(*sql.Tx) error) error {
	err := r.tx(ctx, f)
	if err == nil {
		r.changed()
	}
	return err
}

// Instructions streams the session's instructions with a number greater
// than after, then those queued later. A prompt is recorded as Delivered
// when it is handed to the receiver for the first time. The channel is
// closed when ctx ends.
func (r *Registry) Instructions(ctx context.Context, name string, after uint64) (<-chan Instruction, error) {
	if err := r.exists(ctx, name); err != nil {
		return nil, err
	}
	// What the host has carried out it has been handed, so Cancel no longer
	// drops it.
	if _, err := r.db.ExecContext(ctx, `UPDATE instructions SET sent = 1 WHERE session = ? AND seq <= ?`, name, after); err != nil {
		return nil, err
	}
	wake, stop := r.wait()
	out := make(chan Instruction)
	go func() {
		defer stop()
		defer close(out)
		cursor := after
		for {
			pending, err := r.pending(ctx, name, cursor)
			if err != nil {
				return
			}
			for _, p := range pending {
				select {
				case out <- p.Instruction:
				case <-ctx.Done():
					return
				}
				cursor = p.Seq
				if err := r.markSent(ctx, name, p); err != nil {
					return
				}
			}
			select {
			case <-wake:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

type pendingInstruction struct {
	Instruction
	eventSeq uint64
	sent     bool
}

func (r *Registry) pending(ctx context.Context, name string, after uint64) ([]pendingInstruction, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT seq, kind, source, text, request_id, option_id, event_seq, sent
		FROM instructions WHERE session = ? AND seq > ? ORDER BY seq`, name, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pendingInstruction
	for rows.Next() {
		var p pendingInstruction
		if err := rows.Scan(&p.Seq, &p.Kind, &p.Source, &p.Text, &p.RequestID, &p.OptionID, &p.eventSeq, &p.sent); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (r *Registry) markSent(ctx context.Context, name string, p pendingInstruction) error {
	if p.sent {
		return nil
	}
	err := r.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE instructions SET sent = 1 WHERE session = ? AND seq = ?`, name, p.Seq); err != nil {
			return err
		}
		if p.Kind != InstructionPrompt {
			return nil
		}
		_, err := r.appendEvent(tx, name, Prompt{Source: p.Source, Text: p.Text, State: Delivered, Of: p.eventSeq})
		return err
	})
	if err == nil {
		r.changed()
	}
	return err
}

// Report stores the frames the host sends and returns the number of the
// last host frame stored. A frame at or below that number is ignored, so a
// host that sends frames again after a failed call does no harm; one that
// skips a number is refused with ErrFrameGap and nothing of the call is
// stored. The agent's successful answer to session/new makes a Starting
// session Running.
func (r *Registry) Report(ctx context.Context, name string, frames []HostFrame) (uint64, error) {
	var last uint64
	err := r.tx(ctx, func(tx *sql.Tx) error {
		var state State
		var newReq string
		if err := tx.QueryRow(`SELECT host_seq, state, new_req FROM sessions WHERE name = ?`, name).Scan(&last, &state, &newReq); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		running := false
		for _, f := range frames {
			if f.Seq <= last {
				continue
			}
			if f.Seq != last+1 {
				return fmt.Errorf("%w: frame %d after %d", ErrFrameGap, f.Seq, last)
			}
			if _, err := r.appendEvent(tx, name, Frame{Time: f.Time, Direction: f.Direction, Message: f.Message}); err != nil {
				return err
			}
			last = f.Seq
			if state != Starting {
				continue
			}
			m := parseRPC(f.Message)
			switch {
			case f.Direction == ToAgent && m.Method == "session/new":
				newReq = m.ID
			case f.Direction == FromAgent && m.Method == "" && m.ID != "" && m.ID == newReq && m.OK:
				running = true
			}
		}
		if _, err := tx.Exec(`UPDATE sessions SET host_seq = ?, new_req = ? WHERE name = ?`, last, newReq, name); err != nil {
			return err
		}
		if running {
			return r.setState(tx, name, Running)
		}
		return nil
	})
	if err == nil {
		r.changed()
	}
	return last, err
}

// WaitRunning returns once the session is Running, that is once its host has
// attached and the agent has answered session/new, and ErrNotRunning if the session
// ends first.
func (r *Registry) WaitRunning(ctx context.Context, name string) error {
	wake, stop := r.wait()
	defer stop()
	for {
		var state State
		if err := r.db.QueryRowContext(ctx, `SELECT state FROM sessions WHERE name = ?`, name).Scan(&state); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		switch state {
		case Running:
			return nil
		case Ended:
			return ErrNotRunning
		}
		select {
		case <-wake:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// PutBundle keeps what the session host fetches once before it starts the
// agent: the manifest and the archive as the server built them, both opaque
// to the registry. A start of the session discards the earlier bundle.
func (r *Registry) PutBundle(ctx context.Context, name string, manifest, archive []byte) error {
	if err := r.exists(ctx, name); err != nil {
		return err
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO bundles (session, manifest, archive) VALUES (?, ?, ?)
		ON CONFLICT (session) DO UPDATE SET manifest = excluded.manifest, archive = excluded.archive`,
		name, manifest, archive)
	return err
}

// Bundle returns what PutBundle kept for the session's latest start, and
// ErrNotFound when it kept nothing.
func (r *Registry) Bundle(ctx context.Context, name string) (manifest, archive []byte, err error) {
	err = r.db.QueryRowContext(ctx, `SELECT manifest, archive FROM bundles WHERE session = ?`, name).Scan(&manifest, &archive)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	return manifest, archive, err
}

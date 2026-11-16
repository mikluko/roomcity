package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Entry is an event of a session with the time the server recorded it.
type Entry struct {
	Time  time.Time
	Event Event
}

// Card describes a session in its transcript.
type Card struct {
	Info
	// Began is the time of the session's first event, and Ended the time it
	// stopped; Ended is zero for a session that has not.
	Began, Ended time.Time
}

// Transcript returns a session's card and the whole of its stream, in order:
// the frames as they passed through the server, the prompts at each state
// they reached, and the changes in the session's life. It is what the
// registry holds, so it reaches back over starts afresh, since the numbering
// of a name goes on across them.
func (r *Registry) Transcript(ctx context.Context, name string) (Card, []Entry, error) {
	info, err := r.Get(ctx, name)
	if err != nil {
		return Card{}, nil, err
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT seq, time, kind, direction, message, source, text, pstate, of, lkind
		FROM events WHERE session = ? ORDER BY seq`, name)
	if err != nil {
		return Card{}, nil, err
	}
	defer func() { _ = rows.Close() }()
	card := Card{Info: info}
	var entries []Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return Card{}, nil, err
		}
		if card.Began.IsZero() {
			card.Began = e.Time
		}
		if l, ok := e.Event.(Lifecycle); ok && l.Kind == Stopped {
			card.Ended = e.Time
		}
		entries = append(entries, e)
	}
	return card, entries, rows.Err()
}

func scanEntry(rows *sql.Rows) (Entry, error) {
	var (
		seq, kind, direction, source, pstate, of, lkind uint64
		at                                              int64
		message                                         []byte
		text                                            string
	)
	if err := rows.Scan(&seq, &at, &kind, &direction, &message, &source, &text, &pstate, &of, &lkind); err != nil {
		return Entry{}, err
	}
	when := time.Unix(0, at)
	switch kind {
	case eventFrame:
		return Entry{when, Frame{Seq: seq, Time: when, Direction: Direction(direction), Message: message}}, nil
	case eventPrompt:
		return Entry{when, Prompt{Seq: seq, Source: Source(source), Text: text, State: PromptState(pstate), Of: of}}, nil
	case eventLifecycle:
		return Entry{when, Lifecycle{Seq: seq, Kind: LifecycleKind(lkind)}}, nil
	}
	return Entry{}, fmt.Errorf("session: event %d is of kind %d, which this server does not know", seq, kind)
}

// Purge removes the sessions that ended before the given time, with their
// streams, instructions, marks and bundles, and returns how many it removed.
// A session ended when its last event was recorded; one that has not ended is
// never removed.
func (r *Registry) Purge(ctx context.Context, before time.Time) (int, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT s.name FROM sessions s
		WHERE s.state = ? AND COALESCE((SELECT MAX(time) FROM events e WHERE e.session = s.name), 0) < ?`,
		Ended, before.UnixNano())
	if err != nil {
		return 0, err
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return 0, err
		}
		names = append(names, name)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, name := range names {
		gone, err := r.purgeOne(ctx, name, before)
		if err != nil {
			return removed, fmt.Errorf("session: removing %s: %w", name, err)
		}
		if gone {
			removed++
		}
	}
	return removed, nil
}

// purgeOne removes the session if it is still ended, and still last heard of
// before the time, as it was when it was chosen: a name that was started again
// since is not touched. It says whether it removed the session.
func (r *Registry) purgeOne(ctx context.Context, name string, before time.Time) (bool, error) {
	var gone bool
	err := r.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`
			DELETE FROM sessions WHERE name = ? AND state = ?
			AND COALESCE((SELECT MAX(time) FROM events e WHERE e.session = sessions.name), 0) < ?`,
			name, Ended, before.UnixNano())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		gone = true
		for _, table := range []string{"events", "instructions", "marks", "bundles"} {
			if _, err := tx.Exec(`DELETE FROM `+table+` WHERE session = ?`, name); err != nil {
				return err
			}
		}
		return nil
	})
	return gone, err
}

package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"
)

// Entry is an event of a session with the time the server recorded it.
type Entry struct {
	Time  time.Time
	Event Event
}

// Card describes one start of a session in its transcript.
type Card struct {
	Info
	// Began is when the start was recorded, and Ended when it was closed; Ended
	// is zero for a start that has not been.
	Began, Ended time.Time
	// Start is the start the transcript is of, with its number, how it ended,
	// the chat room and gc's id; Starts are all the starts the registry still
	// holds, the oldest first.
	Start  Start
	Starts []Start
}

// Transcript returns the card and the stream of one start of a session, in
// order: the frames as they passed through the server, the prompts at each
// state they reached, and the changes in the session's life. n is the start's
// number; 0 is the latest. A start that was never made, or was removed with
// the transcripts past their term, gives ErrNotFound, as a session that has
// none does.
func (r *Registry) Transcript(ctx context.Context, name string, n int) (Card, []Entry, error) {
	info, err := r.Get(ctx, name)
	if err != nil {
		return Card{}, nil, err
	}
	starts, err := r.Starts(ctx, name)
	if err != nil {
		return Card{}, nil, err
	}
	start := starts[len(starts)-1]
	if n != 0 {
		i := slices.IndexFunc(starts, func(s Start) bool { return s.N == n })
		if i < 0 {
			return Card{}, nil, ErrNotFound
		}
		start = starts[i]
	}
	last := start.LastSeq
	if last == 0 {
		last = ^uint64(0) >> 1
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT seq, time, kind, direction, message, source, text, pstate, of, lkind
		FROM events WHERE session = ? AND seq >= ? AND seq <= ? ORDER BY seq`, name, start.FirstSeq, last)
	if err != nil {
		return Card{}, nil, err
	}
	defer func() { _ = rows.Close() }()
	card := Card{Info: info, Began: start.Began, Ended: start.Ended, Start: start, Starts: starts}
	var entries []Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return Card{}, nil, err
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

// Purge removes what ended before the given time, and returns how many starts
// it removed. A start that is not the session's latest is removed with its
// events once it ended before the time: each start has its term of its own. A
// session that ended is removed whole, with what is left of its starts, its
// instructions, marks and bundles, when its last event was recorded before the
// time; one that has not ended is never removed.
func (r *Registry) Purge(ctx context.Context, before time.Time) (int, error) {
	removed, err := r.purgeStarts(ctx, before)
	if err != nil {
		return removed, err
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT s.name FROM sessions s
		WHERE s.state = ? AND COALESCE((SELECT MAX(time) FROM events e WHERE e.session = s.name), 0) < ?`,
		Ended, before.UnixNano())
	if err != nil {
		return removed, err
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return removed, err
		}
		names = append(names, name)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return removed, err
	}
	for _, name := range names {
		n, err := r.purgeOne(ctx, name, before)
		if err != nil {
			return removed, fmt.Errorf("session: removing %s: %w", name, err)
		}
		removed += n
	}
	return removed, nil
}

// purgeStarts removes the closed starts, but the latest of each session, that
// ended before the time, with their events.
func (r *Registry) purgeStarts(ctx context.Context, before time.Time) (int, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT st.session, st.n, st.first_seq,
			COALESCE(NULLIF(st.last_seq, 0), (SELECT MIN(nx.first_seq) - 1 FROM starts nx WHERE nx.session = st.session AND nx.n > st.n))
		FROM starts st
		WHERE st.ended <> 0 AND st.ended < ?
		AND st.n < (SELECT MAX(n) FROM starts WHERE session = st.session)`, before.UnixNano())
	if err != nil {
		return 0, err
	}
	type old struct {
		session     string
		n           int
		first, last uint64
	}
	var olds []old
	for rows.Next() {
		var o old
		if err := rows.Scan(&o.session, &o.n, &o.first, &o.last); err != nil {
			_ = rows.Close()
			return 0, err
		}
		olds = append(olds, o)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, err
	}
	removed := 0
	for _, o := range olds {
		err := r.tx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.Exec(`DELETE FROM events WHERE session = ? AND seq >= ? AND seq <= ?`, o.session, o.first, o.last); err != nil {
				return err
			}
			_, err := tx.Exec(`DELETE FROM starts WHERE session = ? AND n = ?`, o.session, o.n)
			return err
		})
		if err != nil {
			return removed, fmt.Errorf("session: removing start %d of %s: %w", o.n, o.session, err)
		}
		removed++
	}
	return removed, nil
}

// purgeOne removes the session if it is still ended, and still last heard of
// before the time, as it was when it was chosen: a name that was started again
// since is not touched. It returns how many starts it removed with it.
func (r *Registry) purgeOne(ctx context.Context, name string, before time.Time) (int, error) {
	var gone int
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
		if err := tx.QueryRow(`SELECT COUNT(*) FROM starts WHERE session = ?`, name).Scan(&gone); err != nil {
			return err
		}
		for _, table := range []string{"events", "instructions", "marks", "bundles", "starts"} {
			if _, err := tx.Exec(`DELETE FROM `+table+` WHERE session = ?`, name); err != nil {
				return err
			}
		}
		return nil
	})
	return gone, err
}

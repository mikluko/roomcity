package session

import (
	"context"
	"time"
)

// Activity is what the stream of a session says of what it is doing now.
type Activity struct {
	// Turn is true from a session/prompt request to the agent until the
	// response to it. A turn that waits for a permission answer is a turn
	// that runs.
	Turn bool
	// Last is the time of the latest frame, in either direction; the zero
	// time for a session that has none.
	Last time.Time
}

// activityState is Activity as read up to the event seq.
type activityState struct {
	Activity
	turnID string
	seq    uint64
}

// Activity returns the session's Activity as its frames say. It reads each
// frame once: what it has read it keeps, and a later call reads what came
// after.
func (r *Registry) Activity(ctx context.Context, name string) (Activity, error) {
	if err := r.exists(ctx, name); err != nil {
		return Activity{}, err
	}
	r.mu.Lock()
	st := r.activity[name]
	if st == nil {
		st = &activityState{}
		r.activity[name] = st
	}
	r.mu.Unlock()

	r.activityMu.Lock()
	defer r.activityMu.Unlock()
	var max uint64
	if err := r.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM events WHERE session = ?`, name).Scan(&max); err != nil {
		return Activity{}, err
	}
	if max < st.seq {
		// The session's events were removed and it began again.
		*st = activityState{}
	}
	rows, err := r.db.QueryContext(ctx, `SELECT seq, time, kind, direction, message FROM events WHERE session = ? AND seq > ? ORDER BY seq`, name, st.seq)
	if err != nil {
		return Activity{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var seq, kind, direction uint64
		var at int64
		var msg []byte
		if err := rows.Scan(&seq, &at, &kind, &direction, &msg); err != nil {
			return Activity{}, err
		}
		st.seq = seq
		switch kind {
		case eventLifecycle:
			st.Turn, st.turnID = false, ""
		case eventFrame:
			st.Last = time.Unix(0, at)
			m := parseRPC(msg)
			switch {
			case Direction(direction) == ToAgent && m.Method == "session/prompt":
				st.Turn, st.turnID = true, m.ID
			case Direction(direction) == FromAgent && m.Method == "" && st.Turn && m.ID == st.turnID && m.ID != "":
				st.Turn, st.turnID = false, ""
			}
		}
	}
	if err := rows.Err(); err != nil {
		return Activity{}, err
	}
	return st.Activity, nil
}

// Tail returns the last n events of the session, in order.
func (r *Registry) Tail(ctx context.Context, name string, n int) ([]Event, error) {
	if err := r.exists(ctx, name); err != nil {
		return nil, err
	}
	var max uint64
	if err := r.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM events WHERE session = ?`, name).Scan(&max); err != nil {
		return nil, err
	}
	from := uint64(0)
	if uint64(n) < max {
		from = max - uint64(n)
	}
	return r.eventsAfter(ctx, name, from)
}

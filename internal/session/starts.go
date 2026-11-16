package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Start is one start of a session: from the event that opened it, Started or
// StartedAfresh, to the Stopped that closed it, or to the event before the
// next start's when the session was lost. One name has one numbering of
// events (Seq) and as many starts as it has been started, each with a
// transcript of its own.
type Start struct {
	// N counts the session's starts from 1.
	N int
	// Began is when the start was recorded, Ended when it was closed; the
	// zero time while it is open.
	Began, Ended time.Time
	// How says how it ended: HowStop for a stop, HowReset for one that Reset
	// asked for, HowFailed for a start that did not come up, HowLost for a
	// start whose box was found dead or that the next start found open, a pod
	// lost without a stop. It is "" while the start is open.
	How string
	// Room is the chat room of the start, as its chat side wrote it with
	// SetRoom; "" for none.
	Room string
	// GCID is the id of gc's session bead, GC_SESSION_ID of the start.
	GCID string
	// FirstSeq is the Seq of the event that opened the start. LastSeq is the
	// Seq of the last event of the start, 0 while it is open.
	FirstSeq, LastSeq uint64
}

// Starts returns the session's starts, the oldest first, and ErrNotFound for
// a session that has none.
func (r *Registry) Starts(ctx context.Context, name string) ([]Start, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT n, began, ended, how, room, gc_id, first_seq, last_seq
		FROM starts WHERE session = ? ORDER BY n`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Start
	for rows.Next() {
		var s Start
		var began, ended int64
		if err := rows.Scan(&s.N, &began, &ended, &s.How, &s.Room, &s.GCID, &s.FirstSeq, &s.LastSeq); err != nil {
			return nil, err
		}
		s.Began = time.Unix(0, began)
		if ended != 0 {
			s.Ended = time.Unix(0, ended)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, ErrNotFound
	}
	return out, nil
}

// SetRoom records the chat room of the session's current start.
func (r *Registry) SetRoom(ctx context.Context, name, room string) error {
	res, err := r.db.ExecContext(ctx, `
		UPDATE starts SET room = ? WHERE session = ? AND n = (SELECT MAX(n) FROM starts WHERE session = ?)`,
		room, name, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Shown implements Service.
func (r *Registry) Shown(ctx context.Context, name string, seq uint64) error {
	res, err := r.db.ExecContext(ctx, `UPDATE sessions SET shown = ? WHERE name = ? AND shown < ?`, seq, name, seq)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Either the name is unknown or the cursor is already at seq or past
		// it; only the first is an error.
		return r.exists(ctx, name)
	}
	return nil
}

// ErrNoResetter is returned by Reset of a registry that has not been told how
// to ask gc.
var ErrNoResetter = errors.New("session: no way to ask gc to reset a session")

// SetResetter sets what Reset asks gc to do for a session of the gc id: `gc
// session reset <id>`, which gc answers by stopping the session's runtime and
// starting it again.
func (r *Registry) SetResetter(f func(ctx context.Context, gcID string) error) {
	r.mu.Lock()
	r.resetter = f
	r.mu.Unlock()
}

// Reset implements Service. It marks the session's open start as one that is
// reset, so that the stop that gc makes closes it as "reset", and asks gc.
func (r *Registry) Reset(ctx context.Context, name string) error {
	r.mu.Lock()
	reset := r.resetter
	r.mu.Unlock()
	if reset == nil {
		return ErrNoResetter
	}
	var gcID string
	err := r.tx(ctx, func(tx *sql.Tx) error {
		var state State
		if err := tx.QueryRow(`SELECT state, gc_id FROM sessions WHERE name = ?`, name).Scan(&state, &gcID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if state == Ended {
			return ErrNotRunning
		}
		if gcID == "" {
			return fmt.Errorf("session %s: the server has no id of gc's session bead for it, which `gc session reset` takes: its start did not carry GC_SESSION_ID, as one that began before the server kept it did not; it can be reset once it has been started again", name)
		}
		_, err := tx.Exec(`UPDATE starts SET reset = 1 WHERE session = ? AND ended = 0`, name)
		return err
	})
	if err != nil {
		return err
	}
	if err := reset(ctx, gcID); err != nil {
		_, uerr := r.db.ExecContext(context.WithoutCancel(ctx), `UPDATE starts SET reset = 0 WHERE session = ? AND ended = 0`, name)
		return errors.Join(err, uerr)
	}
	return nil
}

// BackupPath is the copy of the database, as it was, that Open made before
// it migrated it, and "" when Open migrated nothing. It stays until an
// operator removes it: a server of the earlier version runs on it.
func (r *Registry) BackupPath() string { return r.backup }

// backupName is the name of a copy of the database at path made before a
// migration from version: beside it, with the version and the time in it, so
// that no copy of an earlier try is in its way. A copy that exists is never
// replaced and never the reason the server will not start: the name takes a
// number when the second is taken.
func backupName(path string, version int, at time.Time) (string, error) {
	ext := filepath.Ext(path)
	base := fmt.Sprintf("%s.v%d.%s", strings.TrimSuffix(path, ext), version, at.UTC().Format("20060102T150405Z"))
	for i := 0; i < 1000; i++ {
		name := base + ext
		if i > 0 {
			name = fmt.Sprintf("%s-%d%s", base, i, ext)
		}
		_, err := os.Stat(name)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return name, nil
		case err != nil:
			return "", err
		}
	}
	return "", fmt.Errorf("session: every name of a copy of %s for %s is taken", path, base)
}

// copyBeforeMigrating writes SQLite's own consistent copy of the database to
// a name that carries its version and the time, and returns the name. It
// writes to a temporary name and renames, so that a copy that exists is
// whole.
func copyBeforeMigrating(ctx context.Context, db *sql.DB, path string, version int) (string, error) {
	name, err := backupName(path, version, time.Now())
	if err != nil {
		return "", fmt.Errorf("session: naming the copy of %s: %w", path, err)
	}
	tmp := name + ".tmp"
	_ = os.Remove(tmp)
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, tmp); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("session: copying %s to %s before migrating it: %w", path, name, err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, name); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return name, nil
}

// migrate brings a database of the earlier schema version up to the current
// one, in one transaction.
func migrate(ctx context.Context, db *sql.DB, from int) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if from < 2 {
		if err := migrate1To2(tx); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

// migrate1To2 adds the gc session id and the shown cursor to the sessions and
// makes a start of each Started or StartedAfresh event the sessions have:
// closed by the Stopped that follows, or "lost" when another start follows it
// and "" while the last is open. The cursor of a session begins at its last
// event, so that nothing already shown is shown again.
func migrate1To2(tx *sql.Tx) error {
	for _, stmt := range []string{
		`ALTER TABLE sessions ADD COLUMN gc_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sessions ADD COLUMN shown INTEGER NOT NULL DEFAULT 0`,
		`UPDATE sessions SET shown = COALESCE((SELECT MAX(seq) FROM events e WHERE e.session = sessions.name), 0)`,
		`CREATE TABLE IF NOT EXISTS starts (
			session   TEXT NOT NULL,
			n         INTEGER NOT NULL,
			began     INTEGER NOT NULL,
			ended     INTEGER NOT NULL DEFAULT 0,
			how       TEXT NOT NULL DEFAULT '',
			room      TEXT NOT NULL DEFAULT '',
			gc_id     TEXT NOT NULL DEFAULT '',
			first_seq INTEGER NOT NULL,
			last_seq  INTEGER NOT NULL DEFAULT 0,
			reset     INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (session, n)
		)`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	rows, err := tx.Query(`SELECT session, seq, time, lkind FROM events WHERE kind = ? ORDER BY session, seq`, eventLifecycle)
	if err != nil {
		return err
	}
	type lifecycle struct {
		session string
		seq     uint64
		time    int64
		kind    LifecycleKind
	}
	var all []lifecycle
	for rows.Next() {
		var l lifecycle
		if err := rows.Scan(&l.session, &l.seq, &l.time, &l.kind); err != nil {
			_ = rows.Close()
			return err
		}
		all = append(all, l)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	// What gc told of each session's start that is running is among its marks:
	// the id of its bead is GC_SESSION_ID, kept for a session whose box lives.
	for _, stmt := range []string{
		`UPDATE sessions SET gc_id = COALESCE((SELECT value FROM marks WHERE marks.session = sessions.name AND marks.key = 'GC_SESSION_ID'), '')`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	n := map[string]int{}
	open := map[string]bool{}
	for _, l := range all {
		switch l.kind {
		case Started, StartedAfresh:
			if open[l.session] {
				if _, err := tx.Exec(`UPDATE starts SET ended = ?, how = 'lost', last_seq = ? WHERE session = ? AND n = ?`, l.time, l.seq-1, l.session, n[l.session]); err != nil {
					return err
				}
			}
			n[l.session]++
			open[l.session] = true
			if _, err := tx.Exec(`INSERT INTO starts (session, n, began, first_seq) VALUES (?, ?, ?, ?)`, l.session, n[l.session], l.time, l.seq); err != nil {
				return err
			}
		case Stopped:
			if open[l.session] {
				if _, err := tx.Exec(`UPDATE starts SET ended = ?, how = 'stop', last_seq = ? WHERE session = ? AND n = ?`, l.time, l.seq, l.session, n[l.session]); err != nil {
					return err
				}
				open[l.session] = false
			}
		}
	}
	// The start that is open is the one the marks speak of.
	_, err = tx.Exec(`UPDATE starts SET gc_id = (SELECT gc_id FROM sessions WHERE sessions.name = starts.session) WHERE ended = 0`)
	return err
}

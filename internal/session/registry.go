package session

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const schemaVersion = 1

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
	name       TEXT PRIMARY KEY,
	agent      TEXT NOT NULL,
	kind       INTEGER NOT NULL,
	state      INTEGER NOT NULL,
	token_hash TEXT NOT NULL,
	host_seq   INTEGER NOT NULL DEFAULT 0,
	inst_seq   INTEGER NOT NULL DEFAULT 0,
	new_req    TEXT NOT NULL DEFAULT '',
	epoch      INTEGER NOT NULL DEFAULT 0,
	rev       INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS events (
	session   TEXT NOT NULL,
	seq       INTEGER NOT NULL,
	time      INTEGER NOT NULL,
	kind      INTEGER NOT NULL,
	direction INTEGER NOT NULL DEFAULT 0,
	message   BLOB,
	source    INTEGER NOT NULL DEFAULT 0,
	text      TEXT NOT NULL DEFAULT '',
	pstate    INTEGER NOT NULL DEFAULT 0,
	of        INTEGER NOT NULL DEFAULT 0,
	lkind     INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (session, seq)
);
CREATE TABLE IF NOT EXISTS marks (
	session TEXT NOT NULL,
	key     TEXT NOT NULL,
	value   TEXT NOT NULL,
	PRIMARY KEY (session, key)
);
CREATE TABLE IF NOT EXISTS bundles (
	session  TEXT PRIMARY KEY,
	manifest BLOB NOT NULL,
	archive  BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS instructions (
	session    TEXT NOT NULL,
	seq        INTEGER NOT NULL,
	kind       INTEGER NOT NULL,
	source     INTEGER NOT NULL DEFAULT 0,
	text       TEXT NOT NULL DEFAULT '',
	request_id TEXT NOT NULL DEFAULT '',
	option_id  TEXT NOT NULL DEFAULT '',
	event_seq  INTEGER NOT NULL DEFAULT 0,
	sent       INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (session, seq)
);
`

const (
	eventFrame = iota + 1
	eventPrompt
	eventLifecycle
)

// Registry keeps the sessions, their numbered events and their queues of
// instructions in a SQLite database, so that all of it survives a restart of
// the server. It implements Service and Authorizer.
type Registry struct {
	db  *sql.DB
	now func() time.Time

	mu    sync.Mutex
	waits map[chan struct{}]struct{}
}

var (
	_ Service    = (*Registry)(nil)
	_ Authorizer = (*Registry)(nil)
)

// Open opens the registry kept in the database file at path, making it when
// it does not exist.
func Open(ctx context.Context, path string) (*Registry, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(FULL)")
	if err != nil {
		return nil, err
	}
	// One connection serializes every write, which is what assigning
	// numbers in order needs.
	db.SetMaxOpenConns(1)
	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		_ = db.Close()
		return nil, err
	}
	if version != 0 && version != schemaVersion {
		_ = db.Close()
		return nil, fmt.Errorf("session: database %s is of schema version %d, this server's is %d", path, version, schemaVersion)
	}
	if _, err := db.ExecContext(ctx, schema+fmt.Sprintf("PRAGMA user_version = %d;", schemaVersion)); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("session: schema: %w", err)
	}
	return &Registry{db: db, now: time.Now, waits: map[chan struct{}]struct{}{}}, nil
}

// Close closes the database.
func (r *Registry) Close() error { return r.db.Close() }

// wait returns a channel that receives after something changed, and the
// function that stops it.
func (r *Registry) wait() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	r.mu.Lock()
	r.waits[ch] = struct{}{}
	r.mu.Unlock()
	return ch, func() {
		r.mu.Lock()
		delete(r.waits, ch)
		r.mu.Unlock()
	}
}

func (r *Registry) changed() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for ch := range r.waits {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Authorize reports whether token is the one Start returned for the latest
// start of the session of that name, and the session has not ended.
func (r *Registry) Authorize(name, token string) bool {
	if token == "" {
		return false
	}
	var want string
	err := r.db.QueryRow(`SELECT token_hash FROM sessions WHERE name = ? AND state <> ?`, name, Ended).Scan(&want)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(want), []byte(hashToken(token))) == 1
}

// Start records a start of the session of that name and returns the token
// it proves itself by. A name the registry already holds is started afresh:
// the queue of instructions is emptied, the host's frames count from 1
// again, the earlier token stops working, and the event numbering goes on.
func (r *Registry) Start(ctx context.Context, name, agent string, kind Kind) (string, error) {
	token, err := newToken()
	if err != nil {
		return "", err
	}
	err = r.tx(ctx, func(tx *sql.Tx) error {
		var one int
		lk := Started
		switch err := tx.QueryRow(`SELECT 1 FROM sessions WHERE name = ?`, name).Scan(&one); {
		case err == nil:
			lk = StartedAfresh
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		if _, err := tx.Exec(`
			INSERT INTO sessions (name, agent, kind, state, token_hash, host_seq, rev)
			VALUES (?, ?, ?, ?, ?, 0, 1)
			ON CONFLICT (name) DO UPDATE SET agent = excluded.agent, kind = excluded.kind,
				state = excluded.state, token_hash = excluded.token_hash, host_seq = 0, inst_seq = 0, new_req = '', epoch = epoch + 1, rev = rev + 1`,
			name, agent, kind, Starting, hashToken(token)); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM instructions WHERE session = ?`, name); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM bundles WHERE session = ?`, name); err != nil {
			return err
		}
		_, err := r.appendEvent(tx, name, Lifecycle{Kind: lk})
		return err
	})
	if err != nil {
		return "", err
	}
	r.changed()
	return token, nil
}

// End records the end of the session.
func (r *Registry) End(ctx context.Context, name string) error {
	err := r.tx(ctx, func(tx *sql.Tx) error {
		if err := r.setState(tx, name, Ended); err != nil {
			return err
		}
		_, err := r.appendEvent(tx, name, Lifecycle{Kind: Stopped})
		return err
	})
	if err == nil {
		r.changed()
	}
	return err
}

func (r *Registry) setState(tx *sql.Tx, name string, state State) error {
	res, err := tx.Exec(`UPDATE sessions SET state = ?, rev = rev + 1 WHERE name = ?`, state, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *Registry) tx(ctx context.Context, f func(*sql.Tx) error) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := f(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// appendEvent stores e under the next Seq of the session and returns it.
func (r *Registry) appendEvent(tx *sql.Tx, name string, e Event) (uint64, error) {
	var seq uint64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(seq), 0) + 1 FROM events WHERE session = ?`, name).Scan(&seq); err != nil {
		return 0, err
	}
	at := r.now().UnixNano()
	var err error
	switch e := e.(type) {
	case Frame:
		if !e.Time.IsZero() {
			at = e.Time.UnixNano()
		}
		_, err = tx.Exec(`INSERT INTO events (session, seq, time, kind, direction, message) VALUES (?, ?, ?, ?, ?, ?)`,
			name, seq, at, eventFrame, e.Direction, e.Message)
	case Prompt:
		_, err = tx.Exec(`INSERT INTO events (session, seq, time, kind, source, text, pstate, of) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			name, seq, at, eventPrompt, e.Source, e.Text, e.State, e.Of)
	case Lifecycle:
		_, err = tx.Exec(`INSERT INTO events (session, seq, time, kind, lkind) VALUES (?, ?, ?, ?, ?)`,
			name, seq, at, eventLifecycle, e.Kind)
	default:
		err = fmt.Errorf("session: unknown event %T", e)
	}
	return seq, err
}

// List returns the sessions, ordered by name.
func (r *Registry) List(ctx context.Context) ([]Info, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT name, agent, kind, state FROM sessions ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Info
	for rows.Next() {
		var i Info
		if err := rows.Scan(&i.Name, &i.Agent, &i.Kind, &i.State); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// Watch implements Service.
func (r *Registry) Watch(ctx context.Context) (<-chan Info, error) {
	wake, stop := r.wait()
	seen := map[string]int64{}
	out := make(chan Info)
	go func() {
		defer stop()
		defer close(out)
		for {
			rows, err := r.db.QueryContext(ctx, `SELECT name, agent, kind, state, rev FROM sessions ORDER BY name`)
			if err != nil {
				return
			}
			var changed []Info
			for rows.Next() {
				var i Info
				var rev int64
				if rows.Scan(&i.Name, &i.Agent, &i.Kind, &i.State, &rev) != nil {
					break
				}
				if seen[i.Name] != rev {
					seen[i.Name] = rev
					changed = append(changed, i)
				}
			}
			err = rows.Err()
			_ = rows.Close()
			if err != nil {
				return
			}
			for _, i := range changed {
				select {
				case out <- i:
				case <-ctx.Done():
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

// Subscribe implements Service.
func (r *Registry) Subscribe(ctx context.Context, name string, from uint64) (<-chan Event, error) {
	if err := r.exists(ctx, name); err != nil {
		return nil, err
	}
	cursor := from
	if from == FromNow {
		if err := r.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM events WHERE session = ?`, name).Scan(&cursor); err != nil {
			return nil, err
		}
	}
	wake, stop := r.wait()
	out := make(chan Event)
	go func() {
		defer stop()
		defer close(out)
		for {
			evs, err := r.eventsAfter(ctx, name, cursor)
			if err != nil {
				return
			}
			for _, e := range evs {
				select {
				case out <- e:
					cursor = e.Number()
				case <-ctx.Done():
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

func (r *Registry) eventsAfter(ctx context.Context, name string, after uint64) ([]Event, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT seq, time, kind, direction, message, source, text, pstate, of, lkind
		FROM events WHERE session = ? AND seq > ? ORDER BY seq`, name, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var (
			seq, kind, direction, source, pstate, of, lkind uint64
			at                                              int64
			message                                         []byte
			text                                            string
		)
		if err := rows.Scan(&seq, &at, &kind, &direction, &message, &source, &text, &pstate, &of, &lkind); err != nil {
			return nil, err
		}
		switch kind {
		case eventFrame:
			out = append(out, Frame{Seq: seq, Time: time.Unix(0, at), Direction: Direction(direction), Message: message})
		case eventPrompt:
			out = append(out, Prompt{Seq: seq, Source: Source(source), Text: text, State: PromptState(pstate), Of: of})
		case eventLifecycle:
			out = append(out, Lifecycle{Seq: seq, Kind: LifecycleKind(lkind)})
		}
	}
	return out, rows.Err()
}

func (r *Registry) exists(ctx context.Context, name string) error {
	var one int
	err := r.db.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE name = ?`, name).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

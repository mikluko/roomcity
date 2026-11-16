package session_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"sarak.sh/mikluko/roomcity/internal/session"
)

func TestEachStartIsRecordedAndClosedByHowItEnded(t *testing.T) {
	ctx := ctxT(t)
	r := open(t, filepath.Join(t.TempDir(), "r.db"))

	_, err := r.Starts(ctx, "s")
	require.ErrorIs(t, err, session.ErrNotFound)

	_, err = r.StartWith(ctx, "s", "host", session.AlwaysOn, session.StartInfo{GCID: "sa-1"})
	require.NoError(t, err)
	got, err := r.Starts(ctx, "s")
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, 1, got[0].N)
	require.Equal(t, "sa-1", got[0].GCID)
	require.Empty(t, got[0].How)
	require.True(t, got[0].Ended.IsZero())
	require.Equal(t, uint64(1), got[0].FirstSeq)
	require.Zero(t, got[0].LastSeq)
	require.False(t, got[0].Began.IsZero())

	// A stop closes it as "stop", at the Stopped event.
	require.NoError(t, r.End(ctx, "s"))
	got, _ = r.Starts(ctx, "s")
	require.Equal(t, "stop", got[0].How)
	require.Equal(t, uint64(2), got[0].LastSeq)
	require.False(t, got[0].Ended.IsZero())

	// A second start, then a third without a stop between: the second was lost.
	_, err = r.StartWith(ctx, "s", "host", session.AlwaysOn, session.StartInfo{GCID: "sa-2"})
	require.NoError(t, err)
	_, err = r.Report(ctx, "s", []session.HostFrame{{Seq: 1, Direction: session.ToAgent, Message: []byte("x")}})
	require.NoError(t, err)
	_, err = r.StartWith(ctx, "s", "host", session.AlwaysOn, session.StartInfo{GCID: "sa-3"})
	require.NoError(t, err)

	got, _ = r.Starts(ctx, "s")
	require.Len(t, got, 3)
	require.Equal(t, []int{1, 2, 3}, []int{got[0].N, got[1].N, got[2].N})
	require.Equal(t, uint64(3), got[1].FirstSeq)
	require.Equal(t, "lost", got[1].How)
	require.Equal(t, got[2].FirstSeq-1, got[1].LastSeq, "up to the event before the next start's")
	require.Equal(t, "sa-3", got[2].GCID)
	require.Empty(t, got[2].How)
	require.Equal(t, uint64(5), got[2].FirstSeq, "one Seq across the starts goes on")
}

func TestTheRoomIsTheCurrentStarts(t *testing.T) {
	ctx := ctxT(t)
	r := open(t, filepath.Join(t.TempDir(), "r.db"))
	require.ErrorIs(t, r.SetRoom(ctx, "s", "!r:x"), session.ErrNotFound)

	_, err := r.Start(ctx, "s", "host", session.AlwaysOn)
	require.NoError(t, err)
	require.NoError(t, r.SetRoom(ctx, "s", "!one:x"))
	require.NoError(t, r.End(ctx, "s"))
	_, err = r.Start(ctx, "s", "host", session.AlwaysOn)
	require.NoError(t, err)
	require.NoError(t, r.SetRoom(ctx, "s", "!two:x"))

	got, err := r.Starts(ctx, "s")
	require.NoError(t, err)
	require.Equal(t, "!one:x", got[0].Room)
	require.Equal(t, "!two:x", got[1].Room)
}

func TestResetMarksTheOpenStartAndAsksGcWithTheIdOfItsBead(t *testing.T) {
	ctx := ctxT(t)
	r := open(t, filepath.Join(t.TempDir(), "r.db"))
	require.ErrorIs(t, r.Reset(ctx, "s"), session.ErrNoResetter)

	var asked []string
	r.SetResetter(func(_ context.Context, id string) error {
		asked = append(asked, id)
		return nil
	})
	require.ErrorIs(t, r.Reset(ctx, "nobody"), session.ErrNotFound)

	_, err := r.StartWith(ctx, "s", "host", session.AlwaysOn, session.StartInfo{GCID: "sa-9"})
	require.NoError(t, err)
	require.NoError(t, r.Reset(ctx, "s"))
	require.Equal(t, []string{"sa-9"}, asked)

	// gc stops the session and starts it again: the start ends as a reset.
	require.NoError(t, r.End(ctx, "s"))
	_, err = r.StartWith(ctx, "s", "host", session.AlwaysOn, session.StartInfo{GCID: "sa-9"})
	require.NoError(t, err)
	got, _ := r.Starts(ctx, "s")
	require.Equal(t, "reset", got[0].How)
	require.Empty(t, got[1].How)

	// A reset whose stop never came, then a start: the start is a reset too,
	// not a lost one.
	require.NoError(t, r.Reset(ctx, "s"))
	_, err = r.StartWith(ctx, "s", "host", session.AlwaysOn, session.StartInfo{GCID: "sa-9"})
	require.NoError(t, err)
	got, _ = r.Starts(ctx, "s")
	require.Equal(t, "reset", got[1].How)

	// An ended session cannot be reset.
	require.NoError(t, r.End(ctx, "s"))
	require.ErrorIs(t, r.Reset(ctx, "s"), session.ErrNotRunning)
}

func TestAResetThatGcRefusesLeavesTheStartAsItWas(t *testing.T) {
	ctx := ctxT(t)
	r := open(t, filepath.Join(t.TempDir(), "r.db"))
	refused := errors.New("gc: no such session")
	r.SetResetter(func(context.Context, string) error { return refused })
	_, err := r.StartWith(ctx, "s", "host", session.AlwaysOn, session.StartInfo{GCID: "sa-9"})
	require.NoError(t, err)

	require.ErrorIs(t, r.Reset(ctx, "s"), refused)
	require.NoError(t, r.End(ctx, "s"))
	got, _ := r.Starts(ctx, "s")
	require.Equal(t, "stop", got[0].How, "not a reset: gc did not take it")

	// A session whose start did not carry gc's id cannot be reset.
	_, err = r.Start(ctx, "t", "host", session.AlwaysOn)
	require.NoError(t, err)
	require.ErrorContains(t, r.Reset(ctx, "t"), "the server has no id of gc's session bead")
}

func TestShownAdvancesNeverBackAndAStartLeavesItWhereItIs(t *testing.T) {
	ctx := ctxT(t)
	r := open(t, filepath.Join(t.TempDir(), "r.db"))
	require.ErrorIs(t, r.Shown(ctx, "nobody", 3), session.ErrNotFound)

	_, err := r.Start(ctx, "s", "host", session.AlwaysOn)
	require.NoError(t, err)
	shown := func() uint64 {
		t.Helper()
		infos, err := r.List(ctx)
		require.NoError(t, err)
		require.Len(t, infos, 1)
		got, err := r.Get(ctx, "s")
		require.NoError(t, err)
		require.Equal(t, infos[0].Shown, got.Shown, "List and Get agree")
		return got.Shown
	}
	require.Zero(t, shown(), "a new session has shown nothing")

	require.NoError(t, r.Shown(ctx, "s", 5))
	require.Equal(t, uint64(5), shown())
	require.NoError(t, r.Shown(ctx, "s", 2))
	require.Equal(t, uint64(5), shown(), "never back")
	require.NoError(t, r.Shown(ctx, "s", 5))
	require.Equal(t, uint64(5), shown())

	require.NoError(t, r.End(ctx, "s"))
	_, err = r.Start(ctx, "s", "host", session.AlwaysOn)
	require.NoError(t, err)
	require.Equal(t, uint64(5), shown(), "a start afresh leaves the chat side's cursor")

	w, err := r.Watch(ctx)
	require.NoError(t, err)
	require.Equal(t, uint64(5), recv(t, w).Shown, "Watch carries it")
}

func TestPromptsAndLifecycleChangesCarryTheTimeTheyWereRecorded(t *testing.T) {
	ctx := ctxT(t)
	r := open(t, filepath.Join(t.TempDir(), "r.db"))
	before := time.Now().Add(-time.Second)
	_, err := r.Start(ctx, "s", "host", session.AlwaysOn)
	require.NoError(t, err)
	_, err = r.Prompt(ctx, "s", session.PromptInput{Text: "hi"})
	require.NoError(t, err)

	evs, err := r.Tail(ctx, "s", 10)
	require.NoError(t, err)
	require.Len(t, evs, 2)
	require.True(t, evs[0].(session.Lifecycle).Time.After(before))
	require.True(t, evs[1].(session.Prompt).Time.After(before))
}

func TestAPurgedSessionLeavesNoStartBehind(t *testing.T) {
	ctx := ctxT(t)
	path := filepath.Join(t.TempDir(), "r.db")
	r := open(t, path)
	_, err := r.Start(ctx, "s", "host", session.AlwaysOn)
	require.NoError(t, err)
	require.NoError(t, r.End(ctx, "s"))
	n, err := r.Purge(ctx, time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, n)
	_, err = r.Starts(ctx, "s")
	require.ErrorIs(t, err, session.ErrNotFound)
}

// version1 is the database of the first schema, as a server of 0.3 left it.
const version1 = `
CREATE TABLE sessions (name TEXT PRIMARY KEY, agent TEXT NOT NULL, kind INTEGER NOT NULL, state INTEGER NOT NULL,
	token_hash TEXT NOT NULL, host_seq INTEGER NOT NULL DEFAULT 0, inst_seq INTEGER NOT NULL DEFAULT 0,
	new_req TEXT NOT NULL DEFAULT '', epoch INTEGER NOT NULL DEFAULT 0, rev INTEGER NOT NULL DEFAULT 0);
CREATE TABLE events (session TEXT NOT NULL, seq INTEGER NOT NULL, time INTEGER NOT NULL, kind INTEGER NOT NULL,
	direction INTEGER NOT NULL DEFAULT 0, message BLOB, source INTEGER NOT NULL DEFAULT 0, text TEXT NOT NULL DEFAULT '',
	pstate INTEGER NOT NULL DEFAULT 0, of INTEGER NOT NULL DEFAULT 0, lkind INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (session, seq));
CREATE TABLE marks (session TEXT NOT NULL, key TEXT NOT NULL, value TEXT NOT NULL, PRIMARY KEY (session, key));
CREATE TABLE bundles (session TEXT PRIMARY KEY, manifest BLOB NOT NULL, archive BLOB NOT NULL);
CREATE TABLE instructions (session TEXT NOT NULL, seq INTEGER NOT NULL, kind INTEGER NOT NULL, source INTEGER NOT NULL DEFAULT 0,
	text TEXT NOT NULL DEFAULT '', request_id TEXT NOT NULL DEFAULT '', option_id TEXT NOT NULL DEFAULT '',
	event_seq INTEGER NOT NULL DEFAULT 0, keep_waiting INTEGER NOT NULL DEFAULT 0, sent INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (session, seq));
PRAGMA user_version = 1;
`

func TestADatabaseOfTheFirstSchemaIsMigratedAndKeepsItsSessions(t *testing.T) {
	ctx := ctxT(t)
	path := filepath.Join(t.TempDir(), "r.db")
	old, err := sql.Open("sqlite", "file:"+path)
	require.NoError(t, err)
	_, err = old.Exec(version1)
	require.NoError(t, err)
	for _, q := range []string{
		`INSERT INTO sessions (name, agent, kind, state, token_hash) VALUES ('host', 'host', 1, 2, 'h'), ('worker', 'worker', 2, 3, 'h')`,
		`INSERT INTO marks (session, key, value) VALUES ('host', 'GC_SESSION_ID', 'sa-wisp-host'), ('host', 'other', 'x')`,
		// host: started, lost and started afresh, still running: seqs 1, 2, 3.
		`INSERT INTO events (session, seq, time, kind, lkind) VALUES ('host', 1, 100, 3, 1), ('host', 3, 300, 3, 2)`,
		`INSERT INTO events (session, seq, time, kind, direction, message) VALUES ('host', 2, 200, 1, 1, 'x')`,
		// worker: started and stopped.
		`INSERT INTO events (session, seq, time, kind, lkind) VALUES ('worker', 1, 400, 3, 1), ('worker', 2, 500, 3, 3)`,
	} {
		_, err = old.Exec(q)
		require.NoError(t, err, q)
	}
	require.NoError(t, old.Close())

	r := open(t, path)
	host, err := r.Starts(ctx, "host")
	require.NoError(t, err)
	require.Len(t, host, 2)
	require.Equal(t, session.Start{N: 1, Began: time.Unix(0, 100), Ended: time.Unix(0, 300), How: "lost", FirstSeq: 1, LastSeq: 2}, host[0])
	require.Equal(t, session.Start{N: 2, Began: time.Unix(0, 300), FirstSeq: 3, GCID: "sa-wisp-host"}, host[1], "the start that runs has the id its session's marks hold")
	worker, err := r.Starts(ctx, "worker")
	require.NoError(t, err)
	require.Equal(t, []session.Start{{N: 1, Began: time.Unix(0, 400), Ended: time.Unix(0, 500), How: "stop", FirstSeq: 1, LastSeq: 2}}, worker)

	infos, err := r.List(ctx)
	require.NoError(t, err)
	require.Equal(t, uint64(3), infos[0].Shown, "what exists is taken as shown: the host's last event")
	require.Equal(t, uint64(2), infos[1].Shown)

	// The migrated registry works: a start afresh numbers on.
	_, err = r.StartWith(ctx, "host", "host", session.AlwaysOn, session.StartInfo{GCID: "sa-1"})
	require.NoError(t, err)
	host, _ = r.Starts(ctx, "host")
	require.Len(t, host, 3)
	require.Equal(t, uint64(4), host[2].FirstSeq)

	// It is of the new version now: opening it again changes nothing.
	require.NoError(t, r.Close())
	r = open(t, path)
	again, err := r.Starts(ctx, "host")
	require.NoError(t, err)
	require.Len(t, again, 3)
}

func TestADatabaseOfALaterSchemaIsRefused(t *testing.T) {
	ctx := ctxT(t)
	path := filepath.Join(t.TempDir(), "r.db")
	require.NoError(t, open(t, path).Close())
	db, err := sql.Open("sqlite", "file:"+path)
	require.NoError(t, err)
	_, err = db.Exec(`PRAGMA user_version = 99`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	_, err = session.Open(ctx, path)
	require.ErrorContains(t, err, "schema version 99")
}

// makeVersion1 writes a database of the first schema with one session at path.
func makeVersion1(t *testing.T, path string) {
	t.Helper()
	old, err := sql.Open("sqlite", "file:"+path)
	require.NoError(t, err)
	_, err = old.Exec(version1)
	require.NoError(t, err)
	_, err = old.Exec(`INSERT INTO sessions (name, agent, kind, state, token_hash) VALUES ('host', 'host', 1, 2, 'h')`)
	require.NoError(t, err)
	_, err = old.Exec(`INSERT INTO events (session, seq, time, kind, lkind) VALUES ('host', 1, 100, 3, 1)`)
	require.NoError(t, err)
	require.NoError(t, old.Close())
}

func userVersion(t *testing.T, path string) (version int, hasStarts bool) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	require.NoError(t, err)
	defer db.Close()
	require.NoError(t, db.QueryRow(`PRAGMA user_version`).Scan(&version))
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'starts'`).Scan(&n))
	return version, n > 0
}

// copiesOf lists the copies of the database at path that were made before a
// migration from version 1.
func copiesOf(t *testing.T, dir string) []string {
	t.Helper()
	found, err := filepath.Glob(filepath.Join(dir, "roomcity.v1.*.db"))
	require.NoError(t, err)
	return found
}

func TestTheServerCopiesTheDatabaseAsItWasBeforeItMigratesIt(t *testing.T) {
	ctx := ctxT(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "roomcity.db")
	makeVersion1(t, path)

	r := open(t, path)
	copyPath := r.BackupPath()
	require.Equal(t, []string{copyPath}, copiesOf(t, dir))
	require.Regexp(t, `roomcity\.v1\.\d{8}T\d{6}Z\.db$`, copyPath, "the version and the time are in its name")

	// The copy is whole and is the database as it was: version 1, no starts, its
	// session there. A server of version 1 runs on it.
	version, hasStarts := userVersion(t, copyPath)
	require.Equal(t, 1, version)
	require.False(t, hasStarts)
	db, err := sql.Open("sqlite", "file:"+copyPath)
	require.NoError(t, err)
	var events int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM events WHERE session = 'host'`).Scan(&events))
	require.NoError(t, db.Close())
	require.Equal(t, 1, events)
	info, err := os.Stat(copyPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	require.NoFileExists(t, copyPath+".tmp")

	// The database itself is migrated.
	version, hasStarts = userVersion(t, path)
	require.Equal(t, 4, version)
	require.True(t, hasStarts)
	starts, err := r.Starts(ctx, "host")
	require.NoError(t, err)
	require.Len(t, starts, 1)

	// Opening it again, as version 2, makes no copy.
	require.NoError(t, r.Close())
	again := open(t, path)
	require.Empty(t, again.BackupPath())
	require.Len(t, copiesOf(t, dir), 1)
}

func TestACopyThatExistsIsNeverInTheWayOfTheServerNorReplaced(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "roomcity.db")
	makeVersion1(t, path)

	// What an earlier try left, under the name of the second this one runs in
	// and under the plain one: the server starts all the same, and touches them not.
	older := filepath.Join(dir, "roomcity.v1.db")
	require.NoError(t, os.WriteFile(older, []byte("an operator's, checked or not"), 0o600))
	now := time.Now().UTC().Format("20060102T150405Z")
	sameSecond := filepath.Join(dir, "roomcity.v1."+now+".db")
	require.NoError(t, os.WriteFile(sameSecond, []byte("a try of this second"), 0o600))

	r := open(t, path)
	require.NotEmpty(t, r.BackupPath())
	require.NotEqual(t, sameSecond, r.BackupPath())
	require.NotEqual(t, older, r.BackupPath())
	for name, want := range map[string]string{older: "an operator's, checked or not", sameSecond: "a try of this second"} {
		got, err := os.ReadFile(name)
		require.NoError(t, err)
		require.Equal(t, want, string(got), "not replaced: %s", name)
	}
	version, _ := userVersion(t, r.BackupPath())
	require.Equal(t, 1, version, "the copy of this try is whole")
	version, _ = userVersion(t, path)
	require.Equal(t, 4, version, "and the database is migrated")
}

func TestAFailedMigrationLeavesTheDatabaseAndNoCopyThatWouldBlockTheNextTry(t *testing.T) {
	ctx := ctxT(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "roomcity.db")
	makeVersion1(t, path)
	// A table the migration cannot make over: one named starts with another shape.
	db, err := sql.Open("sqlite", "file:"+path)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE starts (other TEXT)`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	_, err = session.Open(ctx, path)
	require.Error(t, err)
	require.Empty(t, copiesOf(t, dir), "no copy that would be in the way")
	version, _ := userVersion(t, path)
	require.Equal(t, 1, version, "rolled back")
}

func TestTheCallerSaysHowAStartEnded(t *testing.T) {
	ctx := ctxT(t)
	r := open(t, filepath.Join(t.TempDir(), "r.db"))
	r.SetResetter(func(context.Context, string) error { return nil })
	for i, how := range []string{session.HowStop, session.HowFailed, session.HowLost} {
		name := []string{"a", "b", "c"}[i]
		_, err := r.StartWith(ctx, name, "w", session.Task, session.StartInfo{GCID: "sa-" + name})
		require.NoError(t, err)
		require.NoError(t, r.EndAs(ctx, name, how))
		got, _ := r.Starts(ctx, name)
		require.Equal(t, how, got[0].How, name)
	}
	// End is a stop.
	_, err := r.Start(ctx, "d", "w", session.Task)
	require.NoError(t, err)
	require.NoError(t, r.End(ctx, "d"))
	got, _ := r.Starts(ctx, "d")
	require.Equal(t, "stop", got[0].How)

	// A start that Reset marked ends as a reset when it is stopped or lost, and
	// as failed when it failed: a failure is not made a reset.
	for _, name := range []string{"e", "f", "g"} {
		_, err := r.StartWith(ctx, name, "w", session.Task, session.StartInfo{GCID: "sa-" + name})
		require.NoError(t, err)
		require.NoError(t, r.Reset(ctx, name))
	}
	require.NoError(t, r.EndAs(ctx, "e", session.HowStop))
	require.NoError(t, r.EndAs(ctx, "f", session.HowLost))
	require.NoError(t, r.EndAs(ctx, "g", session.HowFailed))
	for name, want := range map[string]string{"e": "reset", "f": "reset", "g": "failed"} {
		got, _ := r.Starts(ctx, name)
		require.Equal(t, want, got[0].How, name)
	}
}

func TestADatabaseOfSchemaTwoHasItsShownCursorsMovedToTheHead(t *testing.T) {
	ctx := ctxT(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "roomcity.db")
	r := open(t, path)
	for _, name := range []string{"host", "guest"} {
		_, err := r.Start(ctx, name, name, session.AlwaysOn)
		require.NoError(t, err)
	}
	for i := uint64(1); i <= 5; i++ {
		_, err := r.Report(ctx, "guest", []session.HostFrame{{Seq: i, Direction: session.ToAgent, Message: []byte("x")}})
		require.NoError(t, err)
	}
	require.NoError(t, r.Shown(ctx, "guest", 2))
	require.NoError(t, r.Close())

	// What version 2 left: the version, and cursors behind the head.
	db, err := sql.Open("sqlite", "file:"+path)
	require.NoError(t, err)
	_, err = db.Exec(`PRAGMA user_version = 2`)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE sessions SET shown = 0 WHERE name = 'host'`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	r = open(t, path)
	infos, err := r.List(ctx)
	require.NoError(t, err)
	shown := map[string]uint64{}
	for _, i := range infos {
		shown[i.Name] = i.Shown
	}
	require.Equal(t, map[string]uint64{"guest": 6, "host": 1}, shown, "each at its last event: guest's is the Started and five frames")

	version, _ := userVersion(t, path)
	require.Equal(t, 4, version)
	require.Equal(t, filepath.Join(dir, "roomcity.v2."), r.BackupPath()[:len(filepath.Join(dir, "roomcity.v2."))], "the copy of version 2 is made")
	require.NoError(t, r.Close())

	// A database of version 3 is not moved again.
	r = open(t, path)
	require.NoError(t, r.Shown(ctx, "host", 1))
	require.Empty(t, r.BackupPath())
}

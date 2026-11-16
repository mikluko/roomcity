package gcwork_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"sarak.sh/mikluko/roomcity/internal/gchandler"
	"sarak.sh/mikluko/roomcity/internal/gcwork"
	"sarak.sh/mikluko/roomcity/internal/server"
	"sarak.sh/mikluko/roomcity/internal/session"
)

// stub is a gc that answers as the pinned revision's commands do, in the
// shapes read from its source, and keeps what it was run with.
const stub = `#!/bin/sh
echo "$*" >> "$LOG"
case "$1" in
sling) cat > "$LOG.stdin"; echo '{"schema_version":"1","ok":true,"success":true,"target":"worker","bead_id":"bd-7"}' ;;
hook) echo "$HOOK" ;;
bd) case "$2" in
    show) [ -z "$SHOW_FAILS" ] || { echo "bd: no such bead" >&2; exit 1; }
        echo "[{\"id\":\"bd-7\",\"title\":\"count the words\",\"description\":\"in a.txt\",\"status\":\"${STATUS:-in_progress}\",\"notes\":\"nine words\",\"metadata\":{\"gc.work_outcome\":\"${OUTCOME:-no-op}\",\"n\":5,\"flag\":true}}]" ;;
    close) [ -n "$CLOSE_FAILS" ] && { echo "bd: no such bead" >&2; exit 1; } ;;
    esac ;;
esac
exit 0
`

type marks map[string]string

func (m marks) Mark(_ context.Context, name, key string) (string, error) { return m[name+"/"+key], nil }
func (m marks) SetMark(_ context.Context, name, key, v string) error {
	m[name+"/"+key] = v
	return nil
}

func work(t *testing.T, env ...string) (*gcwork.Work, marks, string) {
	t.Helper()
	return workWithLogger(t, nil, env...)
}

func workWithLogger(t *testing.T, log *slog.Logger, env ...string) (*gcwork.Work, marks, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "gc")
	require.NoError(t, os.WriteFile(path, []byte(stub), 0o755))
	logPath := filepath.Join(dir, "log")
	h := gchandler.New(gchandler.Config{CityDir: dir, GcPath: path, Env: append(os.Environ(), append([]string{"LOG=" + logPath}, env...)...), Log: log})
	m := marks{}
	return &gcwork.Work{Gc: h, Marks: m, Log: log}, m, logPath
}

func ran(t *testing.T, log string) []string {
	t.Helper()
	b, err := os.ReadFile(log)
	require.NoError(t, err)
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func TestDispatchSlingsTheTaskToThePoolOnStandardInput(t *testing.T) {
	w, _, log := work(t)
	task := "--count the words\nin a.txt"
	id, err := w.Dispatch(context.Background(), "foreman", task)
	require.NoError(t, err)
	require.Equal(t, "bd-7", id)
	require.Equal(t, []string{"sling worker --stdin --json"}, ran(t, log))
	b, err := os.ReadFile(log + ".stdin")
	require.NoError(t, err)
	require.Equal(t, task, string(b), "no word of the task is a flag")
}

func TestClaimGivesTheTextOfTheTaskAndRemembersIt(t *testing.T) {
	w, m, log := work(t, `HOOK={"action":"work","bead_id":"bd-7"}`)
	id, text, found, err := w.Claim(context.Background(), "worker-1")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "bd-7", id)
	require.Equal(t, "count the words\n\nin a.txt", text)
	require.Equal(t, "bd-7", m["worker-1/"+gcwork.ClaimedMark])
	require.Equal(t, []string{"hook --claim --drain-ack --json", "bd show bd-7 --json"}, ran(t, log))
}

func TestClaimWithNoTaskFindsNoneAndGcIsLeftToEndTheSession(t *testing.T) {
	w, m, _ := work(t, `HOOK={"action":"drain"}`)
	_, _, found, err := w.Claim(context.Background(), "worker-1")
	require.NoError(t, err)
	require.False(t, found)
	require.Empty(t, m)
}

func TestDoneNotesClosesAndDrainsInThatOrderAndForgetsTheTask(t *testing.T) {
	w, m, log := work(t)
	m["worker-1/"+gcwork.ClaimedMark] = "bd-7"
	require.NoError(t, w.Done(context.Background(), "worker-1", "--nine words", server.DoneOptions{}))
	require.Equal(t, []string{`bd update bd-7 --notes=--nine words --metadata {"gc.work_outcome":"no-op"}`, "bd close bd-7 --reason=Done: --nine words", "runtime drain-ack"}, ran(t, log))
	require.Empty(t, m["worker-1/"+gcwork.ClaimedMark])
}

func TestDoneBeforeClaimSaysSoAndRunsNothing(t *testing.T) {
	w, _, log := work(t)
	err := w.Done(context.Background(), "worker-1", "x", server.DoneOptions{})
	require.ErrorContains(t, err, "claim first")
	_, statErr := os.Stat(log)
	require.True(t, os.IsNotExist(statErr))
}

func TestAClosingThatFailsEndsNothingAndKeepsTheTask(t *testing.T) {
	w, m, log := work(t, "CLOSE_FAILS=1")
	m["worker-1/"+gcwork.ClaimedMark] = "bd-7"
	err := w.Done(context.Background(), "worker-1", "x", server.DoneOptions{})
	require.ErrorContains(t, err, "no such bead")
	require.NotContains(t, strings.Join(ran(t, log), "\n"), "drain-ack")
	require.Equal(t, "bd-7", m["worker-1/"+gcwork.ClaimedMark], "done can be tried again")
}

type nudges struct {
	mu   sync.Mutex
	sent []string
}

func (n *nudges) nudge(_ context.Context, session, text string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sent = append(n.sent, session+": "+text)
	return nil
}

type running []session.Info

func (r running) List(context.Context) ([]session.Info, error) { return r, nil }

func TestWaitIsKeptAndTheSessionIsToldWhenTheTaskIsClosedOnce(t *testing.T) {
	w, m, log := work(t, "STATUS=closed")
	n := &nudges{}
	w.Nudge = n.nudge
	w.Sessions = running{{Name: "foreman-1", Agent: "foreman", State: session.Running}}
	ctx := context.Background()

	require.NoError(t, w.Wait(ctx, "foreman-1", "bd-7"))
	require.NoError(t, w.Wait(ctx, "foreman-1", "bd-7"), "twice is once")
	require.Equal(t, "bd-7", m["foreman-1/"+gcwork.WaitsMark])
	require.Equal(t, []string{"bd show bd-7 --json"}, ran(t, log), "wait reads the task once, not gc's own wait")

	w.Poll(ctx)
	require.Equal(t, []string{"foreman-1: Task bd-7 is closed. Read what came of it with the tool result."}, n.sent)
	require.Empty(t, m["foreman-1/"+gcwork.WaitsMark])
	w.Poll(ctx)
	require.Len(t, n.sent, 1, "told once")
}

func TestATaskThatIsNotClosedIsWaitedForStill(t *testing.T) {
	w, m, _ := work(t)
	n := &nudges{}
	w.Nudge = n.nudge
	w.Sessions = running{{Name: "foreman-1", State: session.Running}, {Name: "gone", State: session.Ended}}
	m["gone/"+gcwork.WaitsMark] = "bd-7"
	require.NoError(t, w.Wait(context.Background(), "foreman-1", "bd-7"))
	w.Poll(context.Background())
	require.Empty(t, n.sent)
	require.Equal(t, "bd-7", m["foreman-1/"+gcwork.WaitsMark])
}

func TestAFailedTaskIsToldAsFailedWithItsReason(t *testing.T) {
	w, _, _ := work(t, "STATUS=closed", "OUTCOME=abandoned")
	n := &nudges{}
	w.Nudge = n.nudge
	w.Sessions = running{{Name: "foreman-1", State: session.Running}}
	require.NoError(t, w.Wait(context.Background(), "foreman-1", "bd-7"))
	w.Poll(context.Background())
	require.Len(t, n.sent, 1)
	require.Contains(t, n.sent[0], "Task bd-7 is closed, and it failed: nine words")
}

func TestDoneSaysHowTheTaskEnded(t *testing.T) {
	for name, tc := range map[string]struct {
		opts server.DoneOptions
		want string
		err  string
	}{
		"an answer":   {server.DoneOptions{}, `{"gc.work_outcome":"no-op"}`, ""},
		"a commit":    {server.DoneOptions{Commit: "abc", Branch: "main"}, `{"gc.work_branch":"main","gc.work_commit":"abc","gc.work_outcome":"shipped"}`, ""},
		"could not":   {server.DoneOptions{Blocked: true}, `{"gc.work_outcome":"blocked"}`, ""},
		"half a ship": {server.DoneOptions{Commit: "abc"}, "", "together"},
	} {
		w, m, log := work(t)
		m["worker-1/"+gcwork.ClaimedMark] = "bd-7"
		err := w.Done(context.Background(), "worker-1", "nine", tc.opts)
		if tc.err != "" {
			require.ErrorContains(t, err, tc.err, name)
			continue
		}
		require.NoError(t, err, name)
		require.Contains(t, ran(t, log)[0], tc.want, name)
	}
}

func TestDoneWritesCommitAndBranchAsJSONStrings(t *testing.T) {
	w, m, log := work(t)
	m["worker-1/"+gcwork.ClaimedMark] = "bd-7"
	require.NoError(t, w.Done(context.Background(), "worker-1", "nine", server.DoneOptions{Commit: "1234567", Branch: "true"}))

	line := ran(t, log)[0]
	require.NotContains(t, line, "--set-metadata")
	_, encoded, found := strings.Cut(line, " --metadata ")
	require.True(t, found, line)
	var metadata map[string]any
	require.NoError(t, json.Unmarshal([]byte(encoded), &metadata))
	require.Equal(t, map[string]any{
		"gc.work_outcome": "shipped",
		"gc.work_commit":  "1234567",
		"gc.work_branch":  "true",
	}, metadata)
}

func TestWaitRefusesATaskGcCannotReadAndStoresNothing(t *testing.T) {
	w, m, log := work(t, "SHOW_FAILS=1")
	err := w.Wait(context.Background(), "foreman-1", "bd-404")
	require.ErrorContains(t, err, "no such bead")
	require.Empty(t, m)
	require.Equal(t, []string{"bd show bd-404 --json"}, ran(t, log))
}

func TestPollReadsAtDebugAndAnAgentReadAtInfo(t *testing.T) {
	var logged bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
	w, _, _ := workWithLogger(t, logger)
	w.Sessions = running{{Name: "foreman-1", State: session.Running}}

	require.NoError(t, w.Wait(context.Background(), "foreman-1", "bd-7"))
	require.Contains(t, logged.String(), `level=INFO msg="gc ran"`)
	logged.Reset()
	w.Poll(context.Background())
	require.Contains(t, logged.String(), `level=DEBUG msg="gc ran"`)
	require.NotContains(t, logged.String(), `level=INFO msg="gc ran"`)
}

func TestAFailedPollReadIsQuietAndItsFailureIsLogged(t *testing.T) {
	var logged bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
	w, m, _ := workWithLogger(t, logger, "SHOW_FAILS=1")
	m["foreman-1/"+gcwork.WaitsMark] = "bd-7"
	w.Sessions = running{{Name: "foreman-1", State: session.Running}}

	w.Poll(context.Background())
	require.Contains(t, logged.String(), `level=DEBUG msg="gc ran"`)
	require.Contains(t, logged.String(), `level=WARN msg="reading a task a session waits for"`)
	require.NotContains(t, logged.String(), `level=INFO msg="gc ran"`)
}

func TestATaskIdThatIsNoBeadsIdNeverReachesGc(t *testing.T) {
	w, _, log := work(t)
	for _, id := range []string{"", "--help", "-x", "bd-7,bd-8", "bd-7 --json", "bd 7", "../bd-7", "BD-7", "bd-7\n--flag"} {
		require.ErrorContains(t, w.Wait(context.Background(), "foreman", id), "is not the id of a task", id)
		_, _, _, _, err := w.Result(context.Background(), "foreman", id)
		require.ErrorContains(t, err, "is not the id of a task", id)
	}
	_, statErr := os.Stat(log)
	require.True(t, os.IsNotExist(statErr), "gc was not run")
	for _, id := range []string{"bd-7", "spec-bb1.40", "sandbox-ab3", "gc-12.3"} {
		require.NoError(t, w.Wait(context.Background(), "foreman", id), id)
	}
}

func TestResultIsTheNotesOfAClosedTaskAndNothingBeforeThat(t *testing.T) {
	w, _, _ := work(t, "STATUS=closed")
	closed, status, result, outcome, err := w.Result(context.Background(), "foreman", "bd-7")
	require.NoError(t, err)
	require.True(t, closed)
	require.Equal(t, "closed", status)
	require.Equal(t, "nine words", result)
	require.Equal(t, "no-op", outcome)

	w, _, _ = work(t)
	closed, status, result, outcome, err = w.Result(context.Background(), "foreman", "bd-7")
	require.NoError(t, err)
	require.False(t, closed)
	require.Equal(t, "in_progress", status)
	require.Empty(t, result, "not done yet is no result")
	require.Empty(t, outcome)
}

func TestFailClosesTheClaimedTaskAsAbandonedAndEndsTheSession(t *testing.T) {
	w, m, log := work(t)
	m["worker-1/"+gcwork.ClaimedMark] = "bd-7"
	require.NoError(t, w.Fail(context.Background(), "worker-1", "ended without a result: timed out after 30m0s"))
	require.Equal(t, []string{
		`bd update bd-7 --notes=ended without a result: timed out after 30m0s --metadata {"gc.work_outcome":"abandoned"}`,
		"bd close bd-7 --reason=ended without a result: timed out after 30m0s",
		"runtime drain-ack",
	}, ran(t, log))
	require.Empty(t, m["worker-1/"+gcwork.ClaimedMark])
}

func TestFailClaimsTheTaskASessionNeverClaimedAndAbandonsIt(t *testing.T) {
	w, _, log := work(t, `HOOK={"action":"work","bead_id":"bd-7"}`)
	require.NoError(t, w.Fail(context.Background(), "worker-1", "ended without a result: its model failed 3 turns in a row"))
	got := ran(t, log)
	require.Equal(t, "hook --claim --drain-ack --json", got[0])
	require.Equal(t, "bd show bd-7 --json", got[1])
	require.Contains(t, got[2], "bd update bd-7 --notes=ended without a result")
	require.Contains(t, got[2], `"gc.work_outcome":"abandoned"`)
	require.Equal(t, "runtime drain-ack", got[len(got)-1])
}

func TestFailLeavesASessionWithNoTaskToGcWhichHasDrainedIt(t *testing.T) {
	w, _, log := work(t, `HOOK={"action":"drain"}`)
	require.NoError(t, w.Fail(context.Background(), "worker-1", "x"))
	require.Equal(t, []string{"hook --claim --drain-ack --json"}, ran(t, log))
}

func TestAFailAfterTheWorkersOwnDoneChangesNothingAndANewClaimTakesNoTask(t *testing.T) {
	w, m, log := work(t, `HOOK={"action":"work","bead_id":"bd-8"}`)
	m["worker-1/"+gcwork.ClaimedMark] = "bd-7"
	require.NoError(t, w.Done(context.Background(), "worker-1", "nine", server.DoneOptions{}))
	before := len(ran(t, log))

	require.NoError(t, w.Fail(context.Background(), "worker-1", "ended without a result: timed out"))
	got := ran(t, log)[before:]
	require.Equal(t, []string{"runtime drain-ack"}, got, "the end is asked again, the task is not touched")
	_, _, found, err := w.Claim(context.Background(), "worker-1")
	require.ErrorContains(t, err, "your task is over (no-op)")
	require.False(t, found)
	require.NotContains(t, ran(t, log), "hook --claim --drain-ack --json", "the next worker's task is not taken")
	err = w.Done(context.Background(), "worker-1", "again", server.DoneOptions{})
	require.ErrorContains(t, err, "already over (no-op)")
}

func TestADoneAfterAFailIsToldTheTaskIsOver(t *testing.T) {
	w, m, _ := work(t)
	m["worker-1/"+gcwork.ClaimedMark] = "bd-7"
	require.NoError(t, w.Fail(context.Background(), "worker-1", "ended without a result: timed out"))
	err := w.Done(context.Background(), "worker-1", "nine", server.DoneOptions{})
	require.ErrorContains(t, err, "already over (abandoned)")
}

func TestTheActsOnOneSessionsTaskDoNotRunTogether(t *testing.T) {
	w, m, log := work(t)
	m["worker-1/"+gcwork.ClaimedMark] = "bd-7"
	var doneErr, failErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		doneErr = w.Done(context.Background(), "worker-1", "nine", server.DoneOptions{})
	}()
	go func() { defer wg.Done(); failErr = w.Fail(context.Background(), "worker-1", "timed out") }()
	wg.Wait()
	require.Equal(t, 1, strings.Count(strings.Join(ran(t, log), "\n"), "bd close bd-7"), "the task is closed once")
	require.NoError(t, failErr, "a fail on a task that is over is not an error")
	if doneErr != nil {
		require.ErrorContains(t, doneErr, "already over (abandoned)", "the fail came first")
	}
}

func TestAReasonIsCutOnACharacterAndNeverLeavesAnInvalidOne(t *testing.T) {
	w, m, log := work(t)
	m["worker-1/"+gcwork.ClaimedMark] = "bd-7"
	long := strings.Repeat("я", 300)
	require.NoError(t, w.Done(context.Background(), "worker-1", long, server.DoneOptions{}))
	got := ran(t, log)
	require.Equal(t, "bd close bd-7 --reason=Done: "+strings.Repeat("я", 200)+"...", got[1])
	for _, line := range got {
		require.True(t, utf8.ValidString(line), line)
	}

	// 199 ASCII letters and then a two-byte one: a cut at byte 200 is inside it.
	m["worker-2/"+gcwork.ClaimedMark] = "bd-8"
	require.NoError(t, w.Done(context.Background(), "worker-2", strings.Repeat("a", 199)+"я"+"b", server.DoneOptions{}))
	for _, line := range ran(t, log) {
		require.True(t, utf8.ValidString(line), line)
	}

	// What the server writes as a reason is valid too, whatever the model's
	// error held.
	m["worker-3/"+gcwork.ClaimedMark] = "bd-9"
	require.NoError(t, w.Fail(context.Background(), "worker-3", "ended without a result: the last with: \xd1"+strings.Repeat("я", 300)))
	for _, line := range ran(t, log) {
		require.True(t, utf8.ValidString(line), line)
	}
}

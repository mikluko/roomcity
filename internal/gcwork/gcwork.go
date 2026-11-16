// Package gcwork is the one place where the room's four tools for handing work
// to workers, dispatch, wait, claim and done, become gc's commands.
package gcwork

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"sarak.sh/mikluko/roomcity/internal/gchandler"
	"sarak.sh/mikluko/roomcity/internal/server"
	"sarak.sh/mikluko/roomcity/internal/session"
)

// WaitsMark is the session's mark that holds, by commas, the tasks it asked
// the server to watch, each as its id or, once the session has been told that
// it is closed, as the id, "@" and the Seq of the telling's Stored record.
const WaitsMark = session.WaitsMark

// ClaimedMark is the session's mark that holds the task it claimed, for done.
const ClaimedMark = "roomcity.claimed_task"

// OverMark is the session's mark that says its task is over, by the outcome
// it ended with. Done, Fail and Claim are serialized for a session and none
// of them acts on a task that is over, so that a Fail beside a Done cannot
// overwrite its result, and a session that is not yet stopped cannot take
// the next worker's task.
const OverMark = "roomcity.task_over"

// beadID is the shape of a bead's id: a prefix and a suffix, of lowercase
// letters, digits, dots and dashes, never beginning with a dash. A task id
// that came from an agent is checked against it before it is a word of gc's
// command line, where a dash would make it a flag and a comma a list.
var beadID = regexp.MustCompile(`^[a-z0-9][a-z0-9.]*(-[a-z0-9.]+)+$`)

func checkID(id string) error {
	if !beadID.MatchString(id) {
		return fmt.Errorf("%q is not the id of a task: it is what dispatch gave", id)
	}
	return nil
}

// Gc runs gc as a session, with the identity the server kept of its start.
type Gc interface {
	Exec(ctx context.Context, session string, args []string, stdin []byte) (gchandler.Result, error)
}

// Marks keeps a value for a session.
type Marks interface {
	Mark(ctx context.Context, name, key string) (string, error)
	SetMark(ctx context.Context, name, key, value string) error
}

// Work carries out the tools. Pool is the name of the agent whose sessions
// take the tasks; "worker" when empty.
type Work struct {
	Gc    Gc
	Marks Marks
	Pool  string
	// Sessions and Nudge are what telling a session about a waited task needs:
	// the running sessions, and a prompt marked as gc's for one of them, whose
	// Stored Seq Nudge returns. PromptEnd says how the telling ended.
	Sessions interface {
		List(ctx context.Context) ([]session.Info, error)
		PromptEnd(ctx context.Context, name string, seq uint64) (session.PromptState, error)
	}
	Nudge func(ctx context.Context, session, text string) (uint64, error)
	// Log gets what Poll cannot do; nil is slog.Default.
	Log *slog.Logger

	locks        sync.Map // session name -> *sync.Mutex
	readFailures sync.Map // waitedTask -> readFailureState
}

type waitedTask struct {
	session, task string
}

type readFailureState struct {
	missing int
	warned  bool
}

const maxWaitMissingAnswers = 3

// lock serializes the acts on one session's task and returns the unlock.
func (w *Work) lock(session string) func() {
	m, _ := w.locks.LoadOrStore(session, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func (w *Work) pool() string {
	if w.Pool == "" {
		return "worker"
	}
	return w.Pool
}

type gcExec func(ctx context.Context, session string, args []string, stdin []byte) (gchandler.Result, error)

type quietGc interface {
	ExecQuiet(ctx context.Context, session string, args []string, stdin []byte) (gchandler.Result, error)
}

// gc runs one command and returns what it printed, or the reason it failed in
// gc's own words, for the agent to read.
func (w *Work) gc(ctx context.Context, session string, stdin []byte, args ...string) ([]byte, error) {
	return w.exec(ctx, w.Gc.Exec, session, stdin, args...)
}

func (w *Work) gcQuiet(ctx context.Context, session string, stdin []byte, args ...string) ([]byte, error) {
	run := w.Gc.Exec
	if gc, ok := w.Gc.(quietGc); ok {
		run = gc.ExecQuiet
	}
	return w.exec(ctx, run, session, stdin, args...)
}

func (w *Work) exec(ctx context.Context, run gcExec, session string, stdin []byte, args ...string) ([]byte, error) {
	res, err := run(ctx, session, args, stdin)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return res.Stdout, fmt.Errorf("gc %s failed (exit %d): %s", strings.Join(args[:min(len(args), 2)], " "), res.ExitCode, reason(res))
	}
	return res.Stdout, nil
}

func reason(res gchandler.Result) string {
	text := strings.TrimSpace(string(res.Stderr))
	if text == "" {
		text = strings.TrimSpace(string(res.Stdout))
	}
	if len(text) > 500 {
		text = text[:500] + "..."
	}
	return text
}

// Dispatch gives task to the pool and returns the id of the bead that is it.
// The task goes on gc's standard input, so that no word of it is taken for a
// flag.
func (w *Work) Dispatch(ctx context.Context, session, task string) (string, error) {
	out, err := w.gc(ctx, session, []byte(task), "sling", w.pool(), "--stdin", "--json")
	if err != nil {
		return "", err
	}
	var rec struct {
		Success bool   `json:"success"`
		BeadID  string `json:"bead_id"`
	}
	if err := lastRecord(out, &rec); err != nil {
		return "", fmt.Errorf("gc sling answered nothing that can be read: %w", err)
	}
	if !rec.Success || rec.BeadID == "" {
		return "", errors.New("gc did not take the task: it made no bead of it")
	}
	return rec.BeadID, nil
}

// Wait registers the session's wish to be told when the task is closed. An id
// gc cannot read is refused and nothing is stored. The wait survives a new
// start of the session. The telling is the server's own, a prompt marked as
// gc's that Poll queues for the session when the task is closed: gc's own wait
// is a reminder that comes at the session's next boundary, and an idle session
// has none.
func (w *Work) Wait(ctx context.Context, session, task string) error {
	if err := checkID(task); err != nil {
		return err
	}
	defer w.lock(session)()
	waits, err := w.waits(ctx, session)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(waits, func(w string) bool { return waitedID(w) == task }) {
		return nil
	}
	if _, _, _, _, err := w.result(ctx, session, task); err != nil {
		return err
	}
	return w.Marks.SetMark(ctx, session, WaitsMark, strings.Join(append(waits, task), ","))
}

func (w *Work) waits(ctx context.Context, session string) ([]string, error) {
	v, err := w.Marks.Mark(ctx, session, WaitsMark)
	if err != nil || v == "" {
		return nil, err
	}
	return strings.Split(v, ","), nil
}

// Run polls for the closed tasks that sessions wait for every interval, ten
// seconds when zero, until ctx ends.
func (w *Work) Run(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = 10 * time.Second
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			w.Poll(ctx)
		}
	}
}

// waitedID is the task id of an entry of WaitsMark.
func waitedID(entry string) string {
	id, _, _ := strings.Cut(entry, "@")
	return id
}

// told is the Seq of the telling an entry of WaitsMark carries, 0 for none.
func told(entry string) uint64 {
	_, seq, ok := strings.Cut(entry, "@")
	if !ok {
		return 0
	}
	n, _ := strconv.ParseUint(seq, 10, 64)
	return n
}

// Poll tells each running session of a waited task that is closed, once while
// the telling is pending, and ends the wait when the agent has taken the
// telling; a telling that is dropped is told again. A wait whose task gc has
// answered three times in a row with a JSON error object is dropped after a
// telling that says so. Any other failed read keeps the wait and warns once
// until a read succeeds.
func (w *Work) Poll(ctx context.Context) {
	log := w.Log
	if log == nil {
		log = slog.Default()
	}
	all, err := w.Sessions.List(ctx)
	if err != nil {
		log.Error("listing the sessions for the tasks they wait for", "err", err)
		return
	}
	for _, info := range all {
		if info.State != session.Running {
			continue
		}
		w.pollSession(ctx, log, info.Name)
	}
}

func (w *Work) pollSession(ctx context.Context, log *slog.Logger, name string) {
	defer w.lock(name)()
	waits, err := w.waits(ctx, name)
	if err != nil || len(waits) == 0 {
		return
	}
	var still []string
	for _, entry := range waits {
		if keep, kept := w.pollWait(ctx, log, name, entry); keep {
			still = append(still, kept)
		}
	}
	if joined := strings.Join(still, ","); joined != strings.Join(waits, ",") {
		if err := w.Marks.SetMark(ctx, name, WaitsMark, joined); err != nil {
			log.Error("recording the tasks a session still waits for", "session", name, "err", err)
		} else {
			w.forgetRemovedReadFailures(name, still)
		}
	}
}

// pollWait reports whether the wait of one entry goes on, and the entry it is
// then recorded as.
func (w *Work) pollWait(ctx context.Context, log *slog.Logger, name, entry string) (bool, string) {
	id := waitedID(entry)
	if seq := told(entry); seq != 0 {
		end, err := w.Sessions.PromptEnd(ctx, name, seq)
		switch {
		case errors.Is(err, session.ErrNotFound) || err == nil && end == session.Dropped:
			entry = id
		case err != nil:
			log.Error("reading how the telling of a closed task ended", "session", name, "task", id, "err", err)
			return true, entry
		case end == session.Delivered:
			return false, ""
		default:
			return true, entry
		}
	}
	key := waitedTask{session: name, task: id}
	closed, _, result, outcome, err := w.pollResult(ctx, name, id)
	if err != nil {
		return w.keepAfterReadFailure(ctx, log, key, err), entry
	}
	w.readFailures.Delete(key)
	if !closed {
		return true, entry
	}
	seq, err := w.Nudge(ctx, name, closedText(id, outcome, result))
	if err != nil {
		log.Error("telling a session that its task is closed", "session", name, "task", id, "err", err)
		return true, entry
	}
	return true, id + "@" + strconv.FormatUint(seq, 10)
}

func (w *Work) forgetRemovedReadFailures(name string, waits []string) {
	w.readFailures.Range(func(key, _ any) bool {
		k := key.(waitedTask)
		if k.session == name && !slices.ContainsFunc(waits, func(e string) bool { return waitedID(e) == k.task }) {
			w.readFailures.Delete(key)
		}
		return true
	})
}

// keepAfterReadFailure reports whether a wait remains after a failed read. A
// gc error answer counts towards dropping it; any other failure keeps it and
// warns once until a read succeeds.
func (w *Work) keepAfterReadFailure(ctx context.Context, log *slog.Logger, key waitedTask, readErr error) bool {
	state := readFailureState{}
	if previous, ok := w.readFailures.Load(key); ok {
		state = previous.(readFailureState)
	}
	var missing *missingTaskError
	if !errors.As(readErr, &missing) {
		state.missing = 0
		if !state.warned {
			log.Warn("a waited task read failed", "session", key.session, "task", key.task, "err", readErr)
			state.warned = true
		}
		w.readFailures.Store(key, state)
		return true
	}
	state.missing++
	w.readFailures.Store(key, state)
	if state.missing < maxWaitMissingAnswers {
		return true
	}
	if state.missing == maxWaitMissingAnswers {
		log.Warn("a waited task can no longer be read", "session", key.session, "task", key.task, "err", readErr)
	}
	if _, err := w.Nudge(ctx, key.session, unreadableText(key.task)); err != nil {
		log.Error("telling a session that its task can no longer be read", "session", key.session, "task", key.task, "err", err)
		return true
	}
	return false
}

func unreadableText(id string) string {
	return fmt.Sprintf("Task %s can no longer be read, so it is no longer waited for and its result cannot be read.", id)
}

// closedText is the telling: the task, how it ended, and where the answer is.
func closedText(id, outcome, result string) string {
	if outcome == OutcomeAbandoned {
		return fmt.Sprintf("Task %s is closed, and it failed: %s Read it with the tool result.", id, clip(firstLine(result), maxReason))
	}
	return fmt.Sprintf("Task %s is closed. Read what came of it with the tool result.", id)
}

// Result returns the state of the task and, when it is closed, what Done
// stored in it and how it ended.
func (w *Work) Result(ctx context.Context, session, task string) (closed bool, status, result, outcome string, err error) {
	return w.result(ctx, session, task)
}

func (w *Work) result(ctx context.Context, session, task string) (closed bool, status, result, outcome string, err error) {
	return w.readResult(ctx, session, task, false)
}

func (w *Work) pollResult(ctx context.Context, session, task string) (closed bool, status, result, outcome string, err error) {
	return w.readResult(ctx, session, task, true)
}

type missingTaskError struct {
	err error
}

func (e *missingTaskError) Error() string { return e.err.Error() }
func (e *missingTaskError) Unwrap() error { return e.err }

func (w *Work) readResult(ctx context.Context, session, task string, quiet bool) (closed bool, status, result, outcome string, err error) {
	if err := checkID(task); err != nil {
		return false, "", "", "", err
	}
	var out []byte
	if quiet {
		out, err = w.gcQuiet(ctx, session, nil, "bd", "show", task, "--json")
	} else {
		out, err = w.gc(ctx, session, nil, "bd", "show", task, "--json")
	}
	if err != nil {
		if gcErrorAnswer(out) {
			err = &missingTaskError{err: err}
		}
		return false, "", "", "", err
	}
	var beads []struct {
		Status string `json:"status"`
		Notes  string `json:"notes"`
		// bd types what it is given: a value that reads as a number or a
		// boolean comes back as one.
		Metadata map[string]any `json:"metadata"`
	}
	if err := json.Unmarshal(out, &beads); err != nil || len(beads) == 0 {
		return false, "", "", "", errors.New("bd show answered nothing that can be read")
	}
	b := beads[0]
	if b.Status != "closed" {
		return false, b.Status, "", "", nil
	}
	outcome, _ = b.Metadata["gc.work_outcome"].(string)
	return true, b.Status, b.Notes, outcome, nil
}

func gcErrorAnswer(out []byte) bool {
	var answer map[string]json.RawMessage
	if err := json.Unmarshal(out, &answer); err != nil {
		return false
	}
	_, ok := answer["error"]
	return ok
}

// Claim takes the task the session was started for, and returns its id and
// its text. None is found when there is no task, and gc has then been asked to
// end the session.
func (w *Work) Claim(ctx context.Context, session string) (id, text string, found bool, err error) {
	defer w.lock(session)()
	return w.claim(ctx, session)
}

func (w *Work) claim(ctx context.Context, session string) (id, text string, found bool, err error) {
	if outcome, err := w.Marks.Mark(ctx, session, OverMark); err != nil || outcome != "" {
		if err != nil {
			return "", "", false, err
		}
		return "", "", false, fmt.Errorf("your task is over (%s): the session is ending", outcome)
	}
	out, err := w.gc(ctx, session, nil, "hook", "--claim", "--drain-ack", "--json")
	var rec struct {
		Action string `json:"action"`
		BeadID string `json:"bead_id"`
	}
	if perr := lastRecord(out, &rec); perr != nil {
		if err != nil {
			return "", "", false, err
		}
		return "", "", false, fmt.Errorf("gc hook answered nothing that can be read: %w", perr)
	}
	switch rec.Action {
	case "drain":
		return "", "", false, nil
	case "work":
	default:
		return "", "", false, fmt.Errorf("gc hook answered with the action %q", rec.Action)
	}
	if rec.BeadID == "" {
		return "", "", false, errors.New("gc hook gave work with no bead")
	}
	shown, err := w.gc(ctx, session, nil, "bd", "show", rec.BeadID, "--json")
	if err != nil {
		return "", "", false, err
	}
	text, err = taskText(shown)
	if err != nil {
		return "", "", false, err
	}
	if err := w.Marks.SetMark(ctx, session, ClaimedMark, rec.BeadID); err != nil {
		return "", "", false, err
	}
	return rec.BeadID, text, true, nil
}

// Done records result in the claimed task, closes it, and has gc end the
// session.
func (w *Work) Done(ctx context.Context, session, result string, o server.DoneOptions) error {
	defer w.lock(session)()
	if over, err := w.over(ctx, session); err != nil || over != "" {
		if err != nil {
			return err
		}
		return fmt.Errorf("your task is already over (%s): the session is ending", over)
	}
	id, err := w.Marks.Mark(ctx, session, ClaimedMark)
	if err != nil {
		return err
	}
	if id == "" {
		return errors.New("you have claimed no task: claim first")
	}
	outcome, meta := OutcomeNoOp, map[string]string(nil)
	switch {
	case o.Blocked:
		outcome = OutcomeBlocked
	case o.Commit != "" && o.Branch != "":
		outcome = OutcomeShipped
		meta = map[string]string{"gc.work_commit": o.Commit, "gc.work_branch": o.Branch}
	case o.Commit != "" || o.Branch != "":
		return errors.New("a commit and its branch are given together or not at all")
	}
	prefix := "Done: "
	if o.Blocked {
		prefix = "Blocked: "
	}
	return w.finish(ctx, session, id, result, outcome, prefix+clip(firstLine(result), maxReason), meta)
}

// Fail ends a session that cannot finish its task: the task, claimed on the
// session's behalf when it never claimed it, is closed as abandoned with
// reason, and gc is asked to end the session. A session with no task left, as
// gc has it, is ended by gc.
func (w *Work) Fail(ctx context.Context, session, reason string) error {
	defer w.lock(session)()
	if over, err := w.over(ctx, session); err != nil || over != "" {
		return err
	}
	id, err := w.Marks.Mark(ctx, session, ClaimedMark)
	if err != nil {
		return err
	}
	if id == "" {
		var found bool
		if id, _, found, err = w.claim(ctx, session); err != nil || !found {
			return err
		}
	}
	return w.finish(ctx, session, id, reason, OutcomeAbandoned, clip(firstLine(reason), maxReason), nil)
}

// The typed work-record outcomes (gc.work_outcome) that the tools set: a task
// a worker carried out with a commit and its branch is shipped; one it carried
// out with an answer and no commit is no-op, gc's "needed no change", which
// gc's gate takes without a commit; one it could not do is blocked; one the
// server ended because its session could not finish is abandoned.
const (
	OutcomeNoOp      = "no-op"
	OutcomeShipped   = "shipped"
	OutcomeBlocked   = "blocked"
	OutcomeAbandoned = "abandoned"
)

// over says by what outcome the session's task is over, "" while it is not. A
// session whose task is over has gc asked again to end it: the end that was
// asked may be the part that failed.
func (w *Work) over(ctx context.Context, session string) (string, error) {
	outcome, err := w.Marks.Mark(ctx, session, OverMark)
	if err != nil || outcome == "" {
		return "", err
	}
	_, err = w.gc(ctx, session, nil, "runtime", "drain-ack")
	return outcome, err
}

// finish records what came of the task, closes it, forgets it and has gc end
// the session.
func (w *Work) finish(ctx context.Context, session, id, notes, outcome, reason string, extra map[string]string) error {
	notes, reason = strings.ToValidUTF8(notes, "\uFFFD"), strings.ToValidUTF8(reason, "\uFFFD")
	metadata := map[string]string{"gc.work_outcome": outcome}
	for key, value := range extra {
		metadata[key] = value
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("encoding the task's outcome: %w", err)
	}
	args := []string{"bd", "update", id, "--notes=" + notes, "--metadata", string(encoded)}
	if _, err := w.gc(ctx, session, nil, args...); err != nil {
		return err
	}
	if _, err := w.gc(ctx, session, nil, "bd", "close", id, "--reason="+reason); err != nil {
		return err
	}
	if err := w.Marks.SetMark(ctx, session, OverMark, outcome); err != nil {
		return err
	}
	if err := w.Marks.SetMark(ctx, session, ClaimedMark, ""); err != nil {
		return err
	}
	_, err = w.gc(ctx, session, nil, "runtime", "drain-ack")
	return err
}

// maxReason is the most runes of a close reason that gc is given.
const maxReason = 200

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}

// clip cuts s to n runes and marks the cut. The cut is on a rune: a character
// cut through makes bd's store refuse the text.
func clip(s string, n int) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "..."
}

// lastRecord reads the last JSON value gc printed into v: a command with
// --json prints one record, a line.
func lastRecord(out []byte, v any) error {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	return json.Unmarshal([]byte(lines[len(lines)-1]), v)
}

// taskText is the title of the bead bd show printed, and its description
// after it when it has one. bd prints a list of one bead.
func taskText(out []byte) (string, error) {
	var beads []struct {
		Title       string `json:"title"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(out, &beads); err != nil || len(beads) == 0 {
		return "", errors.New("bd show answered nothing that can be read")
	}
	text := beads[0].Title
	if d := strings.TrimSpace(beads[0].Description); d != "" {
		text += "\n\n" + d
	}
	return text, nil
}

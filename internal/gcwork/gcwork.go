// Package gcwork is the one place where the room's four tools for handing work
// to workers, dispatch, wait, claim and done, become gc's commands.
package gcwork

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"sarak.sh/mikluko/roomcity/internal/gchandler"
)

// ClaimedMark is the session's mark that holds the task it claimed, for done.
const ClaimedMark = "roomcity.claimed_task"

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
}

func (w *Work) pool() string {
	if w.Pool == "" {
		return "worker"
	}
	return w.Pool
}

// gc runs one command and returns what it printed, or the reason it failed in
// gc's own words, for the agent to read.
func (w *Work) gc(ctx context.Context, session string, stdin []byte, args ...string) ([]byte, error) {
	res, err := w.Gc.Exec(ctx, session, args, stdin)
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

// Wait registers the session's wish to be told when the task is closed. The
// telling is gc's, a nudge to the session, which is named by its identity.
func (w *Work) Wait(ctx context.Context, session, task string) error {
	if err := checkID(task); err != nil {
		return err
	}
	_, err := w.gc(ctx, session, nil, "session", "wait", "--on-beads", task, "--note", fmt.Sprintf("Task %s is closed: its result is what the tool result gives for it.", task))
	return err
}

// Result returns the state of the task and, when it is closed, what Done
// stored in it.
func (w *Work) Result(ctx context.Context, session, task string) (closed bool, status, result string, err error) {
	if err := checkID(task); err != nil {
		return false, "", "", err
	}
	out, err := w.gc(ctx, session, nil, "bd", "show", task, "--json")
	if err != nil {
		return false, "", "", err
	}
	var beads []struct {
		Status string `json:"status"`
		Notes  string `json:"notes"`
	}
	if err := json.Unmarshal(out, &beads); err != nil || len(beads) == 0 {
		return false, "", "", errors.New("bd show answered nothing that can be read")
	}
	b := beads[0]
	if b.Status != "closed" {
		return false, b.Status, "", nil
	}
	return true, b.Status, b.Notes, nil
}

// Claim takes the task the session was started for, and returns its id and
// its text. None is found when there is no task, and gc has then been asked to
// end the session.
func (w *Work) Claim(ctx context.Context, session string) (id, text string, found bool, err error) {
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
func (w *Work) Done(ctx context.Context, session, result string) error {
	id, err := w.Marks.Mark(ctx, session, ClaimedMark)
	if err != nil {
		return err
	}
	if id == "" {
		return errors.New("you have claimed no task: claim first")
	}
	if _, err := w.gc(ctx, session, nil, "bd", "update", id, "--notes="+result); err != nil {
		return err
	}
	if _, err := w.gc(ctx, session, nil, "bd", "close", id); err != nil {
		return err
	}
	if err := w.Marks.SetMark(ctx, session, ClaimedMark, ""); err != nil {
		return err
	}
	_, err = w.gc(ctx, session, nil, "runtime", "drain-ack")
	return err
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

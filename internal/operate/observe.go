package operate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	roomcityv1 "sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1"
	"sarak.sh/mikluko/roomcity/internal/session"
)

// exitHeld is what a fresh_only start answers for a name some box holds, and
// exitNoScreen what peek answers for a name no box holds: gc reads each as the
// answer it is.
const (
	exitHeld     = 73
	exitNoScreen = 66
)

// restartMark is the mark gc sets to ask for a restart of a session.
const restartMark = "GC_RESTART_REQUESTED"

// JobInfo describes a session's Job.
type JobInfo struct {
	Session string
	// ID names this Job and no other that ever held the name, as a
	// Kubernetes UID does.
	ID      string
	Created time.Time
	// Alive is false for a Job whose pod has ended.
	Alive bool
}

// object is the box that holds a session's name: what observe answers and
// kill-object is asked about.
type object struct {
	ID      string `json:"id"`
	Created string `json:"created"`
}

// box is what holds a name: a Job or a process of the server.
type box struct {
	object
	alive   bool
	process bool
	pid     int
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// locks serializes the operations that race on one name: start, stop,
// observe and kill-object.
type locks struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}

func (l *locks) lock(name string) (unlock func()) {
	l.mu.Lock()
	if l.m == nil {
		l.m = map[string]*sync.Mutex{}
	}
	m, ok := l.m[name]
	if !ok {
		m = new(sync.Mutex)
		l.m[name] = m
	}
	l.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// find returns the box that holds the name, live or dead, and false when
// nothing does. A process held by the server is the box of its name, then a
// Job.
func (o *Operator) find(ctx context.Context, name string) (box, bool, error) {
	if o.cfg.Processes != nil {
		if in := o.cfg.Processes.Info(name); in.Held {
			return box{object: object{ID: "p" + strconv.Itoa(in.PID), Created: stamp(in.Started)}, alive: in.Alive, process: true, pid: in.PID}, true, nil
		}
	}
	j, ok, err := o.jobs.Info(ctx, name)
	if err != nil || !ok {
		return box{}, false, err
	}
	return box{object: object{ID: j.ID, Created: stamp(j.Created)}, alive: j.Alive}, true, nil
}

// identity reads the marks that identify the session gc started: what gc
// stored with its start is what observe, inventory and get-meta answer.
func (o *Operator) identity(ctx context.Context, name string) (map[string]string, error) {
	out := map[string]string{}
	for _, k := range identityKeys {
		if k == "GC_CITY_PATH" || k == "GC_BIN" {
			continue
		}
		v, err := o.reg.Mark(ctx, name, k)
		if err != nil {
			return nil, err
		}
		if v != "" {
			out[k] = v
		}
	}
	return out, nil
}

// agentVerdict says whether the session's agent lives, for the process
// names gc asks about: "alive", "dead" or "unknown". Dead is a verdict gc
// acts on, so it is given only where the server can tell. A service run by the
// server has a process table to read. An agent in a pod does not: its program
// is the one the start staged, and a box that runs it under a name gc does
// not ask about is a box gc has the wrong idea of.
func (o *Operator) agentVerdict(ctx context.Context, name string, b box, asked []string) string {
	if !b.alive || len(asked) == 0 {
		return "unknown"
	}
	if b.process {
		return processVerdict(b.pid, asked)
	}
	raw, _, err := o.reg.Bundle(ctx, name)
	if err != nil {
		return "unknown"
	}
	var m roomcityv1.StageManifest
	if proto.Unmarshal(raw, &m) != nil || len(m.Command) == 0 {
		return "unknown"
	}
	for _, n := range agentNames(m.Command[0]) {
		if slices.Contains(asked, n) {
			return "alive"
		}
	}
	return "dead"
}

// agentNames are the names a program runs under in a pod: Claude Code runs as
// claude, and as node or bun when it is the runtime that is run.
func agentNames(program string) []string {
	base := filepath.Base(program)
	if base == "claude" {
		return []string{"claude", "node", "bun"}
	}
	return []string{base}
}

// processVerdict reads the process table for the root process and what it
// started, as gc's own process table does: a process's name is the base name
// of its command or of its first argument, and an interpreter runs the program
// named by its first argument that is no flag.
func processVerdict(pid int, asked []string) string {
	out, err := exec.Command("ps", "-ww", "-eo", "pid=,ppid=,args=").Output()
	if err != nil {
		return "unknown"
	}
	type row struct {
		ppid int
		args []string
	}
	rows := map[int]row{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		p, err1 := strconv.Atoi(f[0])
		pp, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			continue
		}
		rows[p] = row{pp, f[2:]}
	}
	if _, ok := rows[pid]; !ok {
		return "dead"
	}
	under := func(p int) bool {
		for range rows {
			if p == pid {
				return true
			}
			r, ok := rows[p]
			if !ok || r.ppid == p {
				return false
			}
			p = r.ppid
		}
		return false
	}
	for p, r := range rows {
		if !under(p) {
			continue
		}
		for _, n := range programNames(r.args) {
			if slices.Contains(asked, n) {
				return "alive"
			}
		}
	}
	return "dead"
}

// programNames are the names a process of that command line answers to.
func programNames(args []string) []string {
	base := filepath.Base(args[0])
	names := []string{base}
	switch base {
	case "node", "bun", "npx", "deno":
		for _, a := range args[1:] {
			if strings.HasPrefix(a, "-") || a == "run" || a == "exec" || a == "x" {
				continue
			}
			ab := filepath.Base(a)
			names = append(names, ab, strings.TrimSuffix(ab, filepath.Ext(ab)))
			break
		}
	case "kimi-code":
		names = append(names, "kimi")
	}
	return names
}

func (o *Operator) observe(ctx context.Context, name string, stdin []byte) result {
	b, held, err := o.find(ctx, name)
	if err != nil {
		return failf("observe %s: %v", name, err)
	}
	if !held {
		return jsonResult(map[string]any{"state": "absent"})
	}
	ident, err := o.identity(ctx, name)
	if err != nil {
		return failf("observe %s: %v", name, err)
	}
	state := "dead"
	if b.alive {
		state = "running"
	}
	out := map[string]any{"state": state, "object": b.object, "identity": ident}
	if b.alive {
		out["agent"] = o.agentVerdict(ctx, name, b, strings.Fields(string(stdin)))
	}
	if b.process && b.alive {
		out["agent_pid"] = strconv.Itoa(b.pid)
	}
	return jsonResult(out)
}

type inventoryEntry struct {
	Name           string            `json:"name"`
	State          string            `json:"state"`
	Object         object            `json:"object"`
	Agent          string            `json:"agent,omitempty"`
	Attached       bool              `json:"attached"`
	LastActivity   string            `json:"last_activity,omitempty"`
	RestartRequest bool              `json:"restart_requested"`
	Identity       map[string]string `json:"identity"`
}

// inventory lists every name some box holds for the city, from one read of
// the cluster and one of the registry.
func (o *Operator) inventory(ctx context.Context) result {
	jobs, err := o.jobs.List(ctx)
	if err != nil {
		return failf("inventory: %v", err)
	}
	var entries []inventoryEntry
	seen := map[string]bool{}
	add := func(name string, b box) error {
		if seen[name] {
			return fmt.Errorf("the name %q is held twice", name)
		}
		seen[name] = true
		e := inventoryEntry{Name: name, State: "dead", Object: b.object, Attached: false}
		if b.alive {
			e.State = "running"
			e.Agent = "unknown"
		}
		ident, err := o.identity(ctx, name)
		if err != nil {
			return err
		}
		e.Identity = ident
		mark, err := o.reg.Mark(ctx, name, restartMark)
		if err != nil {
			return err
		}
		e.RestartRequest = mark != ""
		if b.alive {
			last := b.object.Created
			if !b.process {
				if act, err := o.reg.Activity(ctx, name); err == nil && !act.Last.IsZero() {
					last = stamp(act.Last)
				}
			}
			e.LastActivity = last
		}
		entries = append(entries, e)
		return nil
	}
	if o.cfg.Processes != nil {
		for _, name := range o.cfg.Processes.Held() {
			in := o.cfg.Processes.Info(name)
			if err := add(name, box{object: object{ID: "p" + strconv.Itoa(in.PID), Created: stamp(in.Started)}, alive: in.Alive, process: true, pid: in.PID}); err != nil {
				return failf("inventory: %v", err)
			}
		}
	}
	for _, j := range jobs {
		if err := add(j.Session, box{object: object{ID: j.ID, Created: stamp(j.Created)}, alive: j.Alive}); err != nil {
			return failf("inventory: %v", err)
		}
	}
	return jsonResult(map[string]any{"complete": true, "sessions": entries})
}

// killObject deletes the box that holds the name if it is the object asked
// for and the mode's condition holds still, and says what became of it.
func (o *Operator) killObject(ctx context.Context, name string, stdin []byte) result {
	var req struct {
		Object   object `json:"object"`
		Mode     string `json:"mode"`
		AgentPID string `json:"agent_pid"`
	}
	if err := json.Unmarshal(stdin, &req); err != nil || req.Object.ID == "" {
		return failf("kill-object %s: the request is not {object, mode}", name)
	}
	b, held, err := o.find(ctx, name)
	if err != nil {
		return failf("kill-object %s: %v", name, err)
	}
	if !held || b.ID != req.Object.ID || b.Created != req.Object.Created {
		return ok("gone")
	}
	switch req.Mode {
	case "corpse":
		if b.alive {
			return ok("live")
		}
	case "zombie":
		if !b.alive {
			return ok("changed")
		}
		declared, err := o.reg.Mark(ctx, name, "GT_PROCESS_NAMES")
		if err != nil {
			return failf("kill-object %s: %v", name, err)
		}
		asked := strings.FieldsFunc(declared, func(r rune) bool { return r == ',' })
		if o.agentVerdict(ctx, name, b, asked) != "dead" || (b.process && req.AgentPID != "" && req.AgentPID != strconv.Itoa(b.pid)) {
			return ok("changed")
		}
	default:
		return failf("kill-object %s: mode %q is not corpse or zombie", name, req.Mode)
	}
	if err := o.remove(ctx, name, b); err != nil {
		return failf("kill-object %s: %v", name, err)
	}
	return ok("killed")
}

// remove ends the box that holds the name, as stop does but for the Stop
// instruction a dead host cannot take.
func (o *Operator) remove(ctx context.Context, name string, b box) error {
	if b.process {
		if err := o.cfg.Processes.Stop(ctx, name); err != nil {
			return err
		}
		return o.reg.ClearMarks(ctx, name)
	}
	if info, err := o.reg.Get(ctx, name); err == nil && info.State != session.Ended {
		if err := o.reg.End(ctx, name); err != nil {
			return err
		}
	} else if err != nil && !errors.Is(err, session.ErrNotFound) {
		return err
	}
	if _, err := o.jobs.DeleteObject(ctx, name, b.ID); err != nil {
		return err
	}
	return o.reg.ClearMarks(ctx, name)
}

func jsonResult(v any) result {
	b, err := json.Marshal(v)
	if err != nil {
		return failf("marshalling an answer: %v", err)
	}
	return ok(string(b))
}

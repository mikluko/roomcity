// Package operate carries out the operations of gc's exec session runtime
// protocol on the server: it is the server's side of RuntimeService.
package operate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"connectrpc.com/connect"

	roomcityv1 "sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1"
	"sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1/roomcityv1connect"
	"sarak.sh/mikluko/roomcity/internal/session"
)

// Exit codes of an operation. gc reads exitUnknown as an operation the
// runtime does not offer and treats it as success, except for start, where
// it is a failure.
const (
	exitOK      = 0
	exitFailure = 1
	exitUnknown = 2
)

// identityKeys are the variables of a session's start environment that gc
// reads back from the session's marks. GT_PROCESS_NAMES is made from the
// start config's process_names when the environment has none.
var identityKeys = []string{"GC_SESSION_ID", "GC_RUNTIME_EPOCH", "GC_INSTANCE_TOKEN", "GT_PROCESS_NAMES", "GC_CITY_PATH", "GC_BIN"}

// Jobs is the Kubernetes side of a session: one Job for each session.
type Jobs interface {
	// Create makes the session's Job. The pod runs the session host with
	// the given environment, and an empty directory at WorkDir.
	Create(ctx context.Context, spec JobSpec) error
	// Delete removes the session's Job and its pod; a Job that does not
	// exist is no error.
	Delete(ctx context.Context, session string) error
	// Alive reports whether the session has a Job whose pod has not ended.
	Alive(ctx context.Context, session string) (bool, error)
	// Running lists the sessions that have a Job whose pod has not ended.
	Running(ctx context.Context) ([]string, error)
}

// JobSpec is what a session's Job is made from.
type JobSpec struct {
	Session string
	Agent   string
	// Env is the environment of the session host and the agent: the
	// session's GC_* variables and the ROOMCITY_* ones.
	Env     map[string]string
	WorkDir string
}

// Config is how the operations run.
type Config struct {
	// ServerURL is the address a session's pod reaches the server at.
	ServerURL string
	// AlwaysOn says whether the agent's sessions are always-on; any other
	// agent's are started for a task.
	AlwaysOn func(agent string) bool
	// StartTimeout is how long start waits for the host to attach and the
	// agent to answer session/new; 0 is 100 seconds, inside gc's 120.
	StartTimeout time.Duration
	// StopGrace is how long stop waits for the host to take the Stop
	// instruction before it deletes the Job; 0 is 5 seconds.
	StopGrace time.Duration
}

// Operator implements roomcityv1connect.RuntimeServiceHandler. It trusts its
// caller: it is gc's command, and it creates and deletes Jobs, so it is
// served on the server's loopback listener for the runtime and on no port a
// pod can reach.
//
// A session is running when it is registered, has not ended and its Job's
// pod lives, whether or not its host is attached at the moment: a restart of
// the server must not make gc start a session that is still up.
type Operator struct {
	reg  *session.Registry
	jobs Jobs
	cfg  Config
}

var _ roomcityv1connect.RuntimeServiceHandler = (*Operator)(nil)

// New returns an Operator over the registry and the Jobs.
func New(reg *session.Registry, jobs Jobs, cfg Config) *Operator {
	if cfg.StartTimeout == 0 {
		cfg.StartTimeout = 100 * time.Second
	}
	if cfg.StopGrace == 0 {
		cfg.StopGrace = 5 * time.Second
	}
	if cfg.AlwaysOn == nil {
		cfg.AlwaysOn = func(string) bool { return false }
	}
	return &Operator{reg: reg, jobs: jobs, cfg: cfg}
}

// result is what an operation hands back to gc.
type result struct {
	stdout string
	err    error
	code   int
}

func ok(stdout string) result { return result{stdout: stdout} }

func failf(format string, a ...any) result {
	return result{err: fmt.Errorf(format, a...), code: exitFailure}
}

// Operate carries out one operation. A failure of the operation is an
// answer with an exit code, never an error of the call.
func (o *Operator) Operate(ctx context.Context, req *connect.Request[roomcityv1.OperateRequest]) (*connect.Response[roomcityv1.OperateResponse], error) {
	m := req.Msg
	if m.Operation == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("no operation"))
	}
	r := o.run(ctx, m)
	out := &roomcityv1.OperateResponse{Stdout: []byte(r.stdout), ExitCode: int32(r.code)}
	if r.err != nil {
		out.Stderr = []byte(r.err.Error() + "\n")
	}
	return connect.NewResponse(out), nil
}

func (o *Operator) run(ctx context.Context, m *roomcityv1.OperateRequest) result {
	args := m.Args
	name := ""
	if len(args) > 0 {
		name = args[0]
	}
	needName := func() *result {
		if name == "" {
			r := failf("%s: no session name", m.Operation)
			return &r
		}
		return nil
	}
	switch m.Operation {
	case "start":
		if r := needName(); r != nil {
			return *r
		}
		return o.start(ctx, name, m.Stdin)
	case "stop":
		if r := needName(); r != nil {
			return *r
		}
		return o.stop(ctx, name)
	case "is-running":
		if r := needName(); r != nil {
			return *r
		}
		alive, err := o.alive(ctx, name)
		if err != nil {
			return failf("is-running: %v", err)
		}
		return ok(fmt.Sprint(alive))
	case "process-alive":
		if r := needName(); r != nil {
			return *r
		}
		alive, err := o.alive(ctx, name)
		if err != nil {
			return failf("process-alive: %v", err)
		}
		return ok(fmt.Sprint(alive))
	case "list-running":
		prefix := ""
		if len(args) > 0 {
			prefix = args[0]
		}
		return o.listRunning(ctx, prefix)
	case "nudge":
		if r := needName(); r != nil {
			return *r
		}
		return o.nudge(ctx, name, string(m.Stdin))
	case "set-meta", "get-meta", "remove-meta":
		if r := needName(); r != nil {
			return *r
		}
		if len(args) < 2 {
			return failf("%s: no key", m.Operation)
		}
		return o.meta(ctx, m.Operation, name, args[1], m.Stdin)
	}
	return result{code: exitUnknown}
}

// alive reports whether the session is registered, has not ended and has a
// Job whose pod lives.
func (o *Operator) alive(ctx context.Context, name string) (bool, error) {
	info, err := o.reg.Get(ctx, name)
	if errors.Is(err, session.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.State == session.Ended {
		return false, nil
	}
	return o.jobs.Alive(ctx, name)
}

func (o *Operator) listRunning(ctx context.Context, prefix string) result {
	infos, err := o.reg.List(ctx)
	if err != nil {
		return failf("list-running: %v", err)
	}
	jobs, err := o.jobs.Running(ctx)
	if err != nil {
		return failf("list-running: %v", err)
	}
	var names []string
	for _, i := range infos {
		if i.State != session.Ended && strings.HasPrefix(i.Name, prefix) && slices.Contains(jobs, i.Name) {
			names = append(names, i.Name)
		}
	}
	sort.Strings(names)
	return ok(strings.Join(names, "\n"))
}

func (o *Operator) nudge(ctx context.Context, name, text string) result {
	if text == "" {
		return ok("")
	}
	if _, err := o.reg.Nudge(ctx, name, text); err != nil {
		return failf("nudge %s: %v", name, err)
	}
	return ok("")
}

func (o *Operator) meta(ctx context.Context, op, name, key string, stdin []byte) result {
	var err error
	switch op {
	case "set-meta":
		err = o.reg.SetMark(ctx, name, key, string(bytes.TrimSuffix(stdin, []byte("\n"))))
	case "remove-meta":
		err = o.reg.RemoveMark(ctx, name, key)
	default:
		var v string
		if v, err = o.reg.Mark(ctx, name, key); err == nil {
			return ok(v)
		}
	}
	if err != nil {
		return failf("%s %s %s: %v", op, name, key, err)
	}
	return ok("")
}

// start makes the session's Job and returns once the session host has
// attached and the agent has answered session/new. A session that does not
// get there in time is ended and its Job deleted.
func (o *Operator) start(ctx context.Context, name string, stdin []byte) result {
	sc, err := parseStartConfig(stdin)
	if err != nil {
		return failf("start %s: %v", name, err)
	}
	if alive, err := o.alive(ctx, name); err != nil {
		return failf("start %s: %v", name, err)
	} else if alive {
		return failf("start %s: session already exists", name)
	}
	manifest, archive, err := sc.bundle()
	if err != nil {
		return failf("start %s: %v", name, err)
	}

	agent := sc.Env["GC_AGENT"]
	kind := session.Task
	if o.cfg.AlwaysOn(agent) {
		kind = session.AlwaysOn
	}
	// A Job left by an earlier start of the name is gone before the new one.
	if err := o.jobs.Delete(ctx, name); err != nil {
		return failf("start %s: %v", name, err)
	}
	token, err := o.reg.Start(ctx, name, agent, kind)
	if err != nil {
		return failf("start %s: %v", name, err)
	}
	fail := func(format string, a ...any) result {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = o.jobs.Delete(cleanup, name)
		_ = o.reg.End(cleanup, name)
		return failf(format, a...)
	}

	if err := o.reg.PutBundle(ctx, name, manifest, archive); err != nil {
		return fail("start %s: %v", name, err)
	}
	if err := o.reg.ClearMarks(ctx, name); err != nil {
		return fail("start %s: %v", name, err)
	}
	marks := map[string]string{}
	for _, k := range identityKeys {
		marks[k] = sc.Env[k]
	}
	if marks["GT_PROCESS_NAMES"] == "" {
		marks["GT_PROCESS_NAMES"] = strings.Join(sc.ProcessNames, ",")
	}
	for k, v := range marks {
		if v == "" {
			continue
		}
		if err := o.reg.SetMark(ctx, name, k, v); err != nil {
			return fail("start %s: %v", name, err)
		}
	}
	if sc.Nudge != "" {
		if _, err := o.reg.Nudge(ctx, name, sc.Nudge); err != nil {
			return fail("start %s: %v", name, err)
		}
	}
	spec := JobSpec{Session: name, Agent: agent, WorkDir: sc.WorkDir, Env: map[string]string{
		"ROOMCITY_SESSION": name,
		"ROOMCITY_TOKEN":   token,
		"ROOMCITY_SERVER":  o.cfg.ServerURL,
		"ROOMCITY_WORKDIR": sc.WorkDir,
	}}
	for k, v := range sc.Env {
		if strings.HasPrefix(k, "GC_") && v != "" {
			spec.Env[k] = v
		}
	}
	if err := o.jobs.Create(ctx, spec); err != nil {
		return fail("start %s: %v", name, err)
	}

	wait, cancel := context.WithTimeout(ctx, o.cfg.StartTimeout)
	defer cancel()
	switch err := o.reg.WaitRunning(wait, name); {
	case err == nil:
		return ok("")
	case errors.Is(err, context.DeadlineExceeded):
		return fail("start %s: the session did not come up within %s", name, o.cfg.StartTimeout)
	default:
		return fail("start %s: %v", name, err)
	}
}

// stop ends the session: its host is told to stop, the session ends, its
// Job is deleted and its marks are forgotten. A session that does not exist
// is stopped all the same.
func (o *Operator) stop(ctx context.Context, name string) result {
	info, err := o.reg.Get(ctx, name)
	switch {
	case errors.Is(err, session.ErrNotFound):
	case err != nil:
		return failf("stop %s: %v", name, err)
	case info.State != session.Ended:
		if seq, err := o.reg.Stop(ctx, name); err == nil {
			grace, cancel := context.WithTimeout(ctx, o.cfg.StopGrace)
			_ = o.reg.WaitSent(grace, name, seq)
			cancel()
		}
		if err := o.reg.End(ctx, name); err != nil {
			return failf("stop %s: %v", name, err)
		}
	}
	if err := o.jobs.Delete(ctx, name); err != nil {
		return failf("stop %s: %v", name, err)
	}
	if err := o.reg.ClearMarks(ctx, name); err != nil {
		return failf("stop %s: %v", name, err)
	}
	return ok("")
}

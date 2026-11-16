package operate_test

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	roomcityv1 "sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1"
	"sarak.sh/mikluko/roomcity/internal/operate"
	"sarak.sh/mikluko/roomcity/internal/session"
)

// recording loads the frames of a real ACP stream: "out" is to the agent.
func recording(t *testing.T) []session.HostFrame {
	t.Helper()
	f, err := os.Open("testdata/counter.acp.jsonl")
	require.NoError(t, err)
	defer f.Close()
	var frames []session.HostFrame
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var line struct {
			Dir string          `json:"dir"`
			Msg json.RawMessage `json:"msg"`
		}
		require.NoError(t, json.Unmarshal(sc.Bytes(), &line))
		d := session.FromAgent
		if line.Dir == "out" {
			d = session.ToAgent
		}
		frames = append(frames, session.HostFrame{Seq: uint64(len(frames) + 1), Direction: d, Message: line.Msg})
	}
	require.NoError(t, sc.Err())
	return frames
}

// fakeJobs is the Kubernetes side: a Job's pod runs whatever onCreate does.
type fakeJobs struct {
	mu       sync.Mutex
	alive    map[string]bool
	created  []operate.JobSpec
	deleted  []string
	onCreate func(operate.JobSpec)
}

func newFakeJobs() *fakeJobs { return &fakeJobs{alive: map[string]bool{}} }

func (j *fakeJobs) Create(_ context.Context, spec operate.JobSpec) error {
	j.mu.Lock()
	j.alive[spec.Session] = true
	j.created = append(j.created, spec)
	f := j.onCreate
	j.mu.Unlock()
	if f != nil {
		f(spec)
	}
	return nil
}

func (j *fakeJobs) Delete(_ context.Context, name string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	delete(j.alive, name)
	j.deleted = append(j.deleted, name)
	return nil
}

func (j *fakeJobs) Alive(_ context.Context, name string) (bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.alive[name], nil
}

func (j *fakeJobs) Running(context.Context) ([]string, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []string
	for n, a := range j.alive {
		if a {
			out = append(out, n)
		}
	}
	return out, nil
}

type fixture struct {
	t    *testing.T
	ctx  context.Context
	path string
	reg  *session.Registry
	jobs *fakeJobs
	op   *operate.Operator
	dir  string
}

func newFixture(t *testing.T, cfg operate.Config) *fixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	f := &fixture{t: t, ctx: ctx, path: filepath.Join(t.TempDir(), "r.db"), jobs: newFakeJobs(), dir: t.TempDir()}
	f.reopen(cfg)
	return f
}

func (f *fixture) reopen(cfg operate.Config) {
	if f.reg != nil {
		require.NoError(f.t, f.reg.Close())
	}
	reg, err := session.Open(f.ctx, f.path)
	require.NoError(f.t, err)
	f.t.Cleanup(func() { _ = reg.Close() })
	f.reg = reg
	if cfg.ServerURL == "" {
		cfg.ServerURL = "http://roomcity.test:8080"
	}
	f.op = operate.New(reg, f.jobs, cfg)
}

func (f *fixture) do(op string, stdin string, args ...string) *roomcityv1.OperateResponse {
	f.t.Helper()
	res, err := f.op.Operate(f.ctx, connect.NewRequest(&roomcityv1.OperateRequest{Operation: op, Args: args, Stdin: []byte(stdin)}))
	require.NoError(f.t, err)
	return res.Msg
}

// startConfig writes a settings file and an overlay under the fixture's
// directory and returns gc's start config naming them.
func (f *fixture) startConfig(extra map[string]any) string {
	f.t.Helper()
	settings := filepath.Join(f.dir, "settings.json")
	require.NoError(f.t, os.WriteFile(settings, []byte(`{"hooks":{}}`), 0o644))
	overlay := filepath.Join(f.dir, "overlay")
	require.NoError(f.t, os.MkdirAll(filepath.Join(overlay, ".claude"), 0o755))
	require.NoError(f.t, os.WriteFile(filepath.Join(overlay, ".claude", "x.md"), []byte("overlay"), 0o644))
	cfg := map[string]any{
		"work_dir":    "/city/.gc/agents/host",
		"command":     "claude --model sonnet --settings " + settings,
		"env":         map[string]string{"GC_AGENT": "host", "GC_SESSION_ID": "gc-7", "GC_CITY_PATH": "/city", "GC_CONTROLLER_TOKEN": "", "HOME": "/ignored"},
		"nudge":       "begin",
		"pre_start":   []string{"true"},
		"overlay_dir": overlay,
	}
	for k, v := range extra {
		cfg[k] = v
	}
	b, err := json.Marshal(cfg)
	require.NoError(f.t, err)
	return string(b)
}

// host is what a session's pod does once it runs: it attaches, carries out
// what the server queues, and reports the recording.
type host struct {
	got    chan session.Instruction
	frames []session.HostFrame
}

func (h *host) run(ctx context.Context, reg *session.Registry, spec operate.JobSpec, attach bool) {
	if attach {
		in, err := reg.Instructions(ctx, spec.Session, 0)
		if err != nil {
			return
		}
		go func() {
			for i := range in {
				h.got <- i
			}
		}()
	}
	for i := 0; i < len(h.frames); i += 20 {
		if _, err := reg.Report(ctx, spec.Session, h.frames[i:min(i+20, len(h.frames))]); err != nil {
			return
		}
	}
}

func TestStartMakesAJobAndReturnsOnlyAfterTheAgentAnswered(t *testing.T) {
	f := newFixture(t, operate.Config{AlwaysOn: func(a string) bool { return a == "host" }})
	h := &host{got: make(chan session.Instruction, 8), frames: recording(t)}
	release := make(chan struct{})
	f.jobs.onCreate = func(spec operate.JobSpec) {
		require.True(t, f.reg.Authorize(spec.Session, spec.Env["ROOMCITY_TOKEN"]))
		go func() {
			<-release
			h.run(f.ctx, f.reg, spec, true)
		}()
	}

	done := make(chan *roomcityv1.OperateResponse, 1)
	go func() { done <- f.do("start", f.startConfig(nil), "host-1") }()
	select {
	case r := <-done:
		t.Fatalf("start returned before the host attached: %+v", r)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	r := <-done
	require.Zero(t, r.ExitCode, string(r.Stderr))
	require.Equal(t, "true", string(f.do("is-running", "", "host-1").Stdout))
	require.Equal(t, "true", string(f.do("process-alive", "", "host-1").Stdout))
	require.Equal(t, "host-1", string(f.do("list-running", "", "").Stdout))
	require.Equal(t, "", string(f.do("list-running", "", "other-").Stdout))

	info, err := f.reg.Get(f.ctx, "host-1")
	require.NoError(t, err)
	require.Equal(t, session.Info{Name: "host-1", Agent: "host", Kind: session.AlwaysOn, State: session.Running}, info)

	require.Len(t, f.jobs.created, 1)
	spec := f.jobs.created[0]
	require.Equal(t, "http://roomcity.test:8080", spec.Env["ROOMCITY_SERVER"])
	require.Equal(t, "/city/.gc/agents/host", spec.Env["ROOMCITY_WORKDIR"])
	require.Equal(t, "gc-7", spec.Env["GC_SESSION_ID"])
	require.NotContains(t, spec.Env, "HOME")
	require.NotContains(t, spec.Env, "GC_CONTROLLER_TOKEN")

	manifest, archive, err := f.reg.Bundle(f.ctx, "host-1")
	require.NoError(t, err)
	var m roomcityv1.StageManifest
	require.NoError(t, proto.Unmarshal(manifest, &m))
	require.Equal(t, []string{"claude", "--model", "sonnet", "--settings", "/city/.gc/agents/host/.roomcity/settings.json"}, m.Command)
	require.Equal(t, []string{"true"}, m.PreStart)
	require.Contains(t, string(archive), ".roomcity/settings.json")
	require.Contains(t, string(archive), ".claude/x.md")

	v, err := f.reg.Mark(f.ctx, "host-1", "GC_SESSION_ID")
	require.NoError(t, err)
	require.Equal(t, "gc-7", v)
}

func TestTheStartConfigsNudgeIsTheFirstPromptAndTheFramesComeOutInOrder(t *testing.T) {
	f := newFixture(t, operate.Config{})
	rec := recording(t)
	h := &host{got: make(chan session.Instruction, 8), frames: rec}
	f.jobs.onCreate = func(spec operate.JobSpec) { go h.run(f.ctx, f.reg, spec, true) }

	r := f.do("start", f.startConfig(nil), "w-1")
	require.Zero(t, r.ExitCode, string(r.Stderr))

	first := <-h.got
	require.Equal(t, session.Instruction{Seq: 1, Kind: session.InstructionPrompt, Source: session.FromGC, Text: "begin"}, first)

	evs, err := f.reg.Subscribe(f.ctx, "w-1", 0)
	require.NoError(t, err)
	var frames []session.Frame
	for e := range evs {
		if fr, ok := e.(session.Frame); ok {
			frames = append(frames, fr)
		}
		if len(frames) == len(rec) {
			break
		}
	}
	require.Len(t, frames, len(rec))
	for i, fr := range frames {
		require.Equal(t, rec[i].Direction, fr.Direction)
		require.JSONEq(t, string(rec[i].Message), string(fr.Message))
		if i > 0 {
			require.Greater(t, fr.Seq, frames[i-1].Seq)
		}
	}
}

func TestANudgeReachesTheHostNumberedAndSurvivesARestartOfTheServer(t *testing.T) {
	f := newFixture(t, operate.Config{})
	rec := recording(t)
	// The host reports but is not attached for instructions yet.
	h := &host{got: make(chan session.Instruction, 8), frames: rec}
	f.jobs.onCreate = func(spec operate.JobSpec) { go h.run(f.ctx, f.reg, spec, false) }

	r := f.do("start", f.startConfig(map[string]any{"nudge": ""}), "w-1")
	require.Zero(t, r.ExitCode, string(r.Stderr))
	r = f.do("nudge", "wake up", "w-1")
	require.Zero(t, r.ExitCode, string(r.Stderr))

	f.reopen(operate.Config{})
	require.Equal(t, "true", string(f.do("is-running", "", "w-1").Stdout), "the Job outlives the server")

	in, err := f.reg.Instructions(f.ctx, "w-1", 0)
	require.NoError(t, err)
	got := <-in
	require.Equal(t, session.Instruction{Seq: 1, Kind: session.InstructionPrompt, Source: session.FromGC, Text: "wake up"}, got)
}

func TestStopSendsStopAndDeletesTheJob(t *testing.T) {
	f := newFixture(t, operate.Config{})
	h := &host{got: make(chan session.Instruction, 8), frames: recording(t)}
	f.jobs.onCreate = func(spec operate.JobSpec) { go h.run(f.ctx, f.reg, spec, true) }
	require.Zero(t, f.do("start", f.startConfig(map[string]any{"nudge": ""}), "w-1").ExitCode)
	require.Zero(t, f.do("set-meta", "1\n", "w-1", "GC_DRAIN").ExitCode)

	r := f.do("stop", "", "w-1")
	require.Zero(t, r.ExitCode, string(r.Stderr))
	require.Equal(t, session.InstructionStop, (<-h.got).Kind)
	require.Equal(t, "false", string(f.do("is-running", "", "w-1").Stdout))
	require.Contains(t, f.jobs.deleted, "w-1")
	require.Empty(t, f.do("get-meta", "", "w-1", "GC_DRAIN").Stdout)
	info, err := f.reg.Get(f.ctx, "w-1")
	require.NoError(t, err)
	require.Equal(t, session.Ended, info.State)

	require.Zero(t, f.do("stop", "", "w-1").ExitCode, "stop is idempotent")
	require.Zero(t, f.do("stop", "", "never-started").ExitCode)
}

func TestAStartThatDoesNotComeUpFailsAndLeavesNothing(t *testing.T) {
	f := newFixture(t, operate.Config{StartTimeout: 150 * time.Millisecond})
	r := f.do("start", f.startConfig(nil), "w-1")
	require.Equal(t, int32(1), r.ExitCode)
	require.Contains(t, string(r.Stderr), "did not come up")
	require.Contains(t, f.jobs.deleted, "w-1")
	require.Equal(t, "false", string(f.do("is-running", "", "w-1").Stdout))
}

func TestStartOfALiveSessionFails(t *testing.T) {
	f := newFixture(t, operate.Config{})
	h := &host{got: make(chan session.Instruction, 8), frames: recording(t)}
	f.jobs.onCreate = func(spec operate.JobSpec) { go h.run(f.ctx, f.reg, spec, true) }
	require.Zero(t, f.do("start", f.startConfig(map[string]any{"nudge": ""}), "w-1").ExitCode)

	r := f.do("start", f.startConfig(nil), "w-1")
	require.Equal(t, int32(1), r.ExitCode)
	require.Contains(t, string(r.Stderr), "already exists")
	require.Len(t, f.jobs.created, 1)
}

func TestMarksOfASessionThatDiedWithoutAStopAreGoneAtItsNextStart(t *testing.T) {
	f := newFixture(t, operate.Config{})
	h := &host{got: make(chan session.Instruction, 8), frames: recording(t)}
	f.jobs.onCreate = func(spec operate.JobSpec) { go h.run(f.ctx, f.reg, spec, true) }
	cfg := f.startConfig(map[string]any{"nudge": "", "process_names": []string{"claude", "node"}})
	require.Zero(t, f.do("start", cfg, "w-1").ExitCode)
	require.Equal(t, "claude,node", string(f.do("get-meta", "", "w-1", "GT_PROCESS_NAMES").Stdout))
	require.Zero(t, f.do("set-meta", "1", "w-1", "GC_DRAIN").ExitCode)

	// The pod is lost; gc finds the session not running and starts it again.
	f.jobs.mu.Lock()
	f.jobs.alive["w-1"] = false
	f.jobs.mu.Unlock()
	require.Equal(t, "false", string(f.do("is-running", "", "w-1").Stdout))
	require.Zero(t, f.do("start", cfg, "w-1").ExitCode)

	require.Empty(t, f.do("get-meta", "", "w-1", "GC_DRAIN").Stdout)
	require.Equal(t, "gc-7", string(f.do("get-meta", "", "w-1", "GC_SESSION_ID").Stdout))
}

func TestAnOperationTheRuntimeDoesNotOfferAnswersUnknown(t *testing.T) {
	f := newFixture(t, operate.Config{})
	for _, op := range []string{"protocol", "peek", "interrupt", "send-keys", "attach", "exec", "get-last-activity"} {
		r := f.do(op, "", "s")
		require.Equal(t, int32(2), r.ExitCode, op)
	}
	require.Equal(t, int32(1), f.do("start", "{", "s").ExitCode)
	require.Equal(t, "false", string(f.do("is-running", "", "never").Stdout))
}

func TestMetaOperations(t *testing.T) {
	f := newFixture(t, operate.Config{})
	require.Empty(t, f.do("get-meta", "", "nobody", "k").Stdout)
	require.Zero(t, f.do("set-meta", "value\n", "s", "k").ExitCode)
	require.Equal(t, "value", string(f.do("get-meta", "", "s", "k").Stdout))
	require.Zero(t, f.do("remove-meta", "", "s", "k").ExitCode)
	require.Empty(t, f.do("get-meta", "", "s", "k").Stdout)
	require.Equal(t, int32(1), f.do("set-meta", "v", "s").ExitCode, "no key")
}

func TestNudge(t *testing.T) {
	f := newFixture(t, operate.Config{})
	require.Zero(t, f.do("nudge", "", "nobody").ExitCode, "empty text is a no-op")
	r := f.do("nudge", "text", "nobody")
	require.Equal(t, int32(1), r.ExitCode)
	require.True(t, strings.Contains(string(r.Stderr), "not found"), string(r.Stderr))
}

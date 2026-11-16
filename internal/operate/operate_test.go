package operate_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
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
	"sarak.sh/mikluko/roomcity/internal/procs"
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
	failure  string
	alive    map[string]bool
	ids      map[string]string
	n        int
	created  []operate.JobSpec
	deleted  []string
	onCreate func(operate.JobSpec)
}

func newFakeJobs() *fakeJobs {
	return &fakeJobs{alive: map[string]bool{}, ids: map[string]string{}}
}

func (j *fakeJobs) Create(_ context.Context, spec operate.JobSpec) error {
	j.mu.Lock()
	j.alive[spec.Session] = true
	j.n++
	j.ids[spec.Session] = fmt.Sprintf("uid-%d", j.n)
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

func (j *fakeJobs) Failure(context.Context, string) (string, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.failure, nil
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
	f := newFixture(t, operate.Config{
		AlwaysOn: func(a string) bool { return a == "host" },
		Tools: func(a string) []string {
			if a == "host" {
				return []string{"dispatch", "wait"}
			}
			return nil
		},
	})
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
	require.Equal(t, "dispatch,wait", spec.Env[operate.ToolsEnv], "the tools of the agent's role, for the host to register")
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

	identity, kept, err := f.reg.Identity(f.ctx, "host-1")
	require.NoError(t, err)
	require.True(t, kept)
	require.Equal(t, "gc-7", identity["GC_SESSION_ID"])
	require.Equal(t, "host", identity["GC_AGENT"])
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
	for _, op := range []string{"attach", "exec", "copy-to", "check-image", "watch-startup", "provision"} {
		r := f.do(op, "", "s")
		require.Equal(t, int32(2), r.ExitCode, op)
	}
	require.Equal(t, int32(1), f.do("start", "{", "s").ExitCode)
	require.Equal(t, "false", string(f.do("is-running", "", "never").Stdout))
}

func TestTheHandshakeDeclaresWhatTheRuntimeDoes(t *testing.T) {
	f := newFixture(t, operate.Config{})
	r := f.do("protocol", "")
	require.Zero(t, r.ExitCode)
	var hs struct {
		Version      int      `json:"version"`
		Capabilities []string `json:"capabilities"`
	}
	require.NoError(t, json.Unmarshal(r.Stdout, &hs))
	require.Zero(t, hs.Version)
	require.ElementsMatch(t, []string{"carrier.ops", "idle-boundary", "observe.v1", "report-activity", "report-attachment"}, hs.Capabilities)
}

// startWithFrames starts a session whose pod reports no frames of its own but
// the ones needed to be running, and returns it.
func startRunning(t *testing.T, f *fixture, name string) {
	t.Helper()
	f.jobs.onCreate = func(spec operate.JobSpec) {
		go func() {
			_, err := f.reg.Report(f.ctx, spec.Session, []session.HostFrame{
				{Seq: 1, Direction: session.ToAgent, Message: []byte(`{"jsonrpc":"2.0","id":1,"method":"session/new"}`)},
				{Seq: 2, Direction: session.FromAgent, Message: []byte(`{"jsonrpc":"2.0","id":1,"result":{"sessionId":"x"}}`)},
			})
			require.NoError(t, err)
		}()
	}
	require.Zero(t, f.do("start", f.startConfig(map[string]any{"nudge": ""}), name).ExitCode)
}

func TestPeekShowsTheScreenAndItsLastLineSaysWhetherATurnRuns(t *testing.T) {
	f := newFixture(t, operate.Config{})
	startRunning(t, f, "w-1")
	report := func(seq uint64, d session.Direction, msg string) {
		_, err := f.reg.Report(f.ctx, "w-1", []session.HostFrame{{Seq: seq, Direction: d, Message: []byte(msg)}})
		require.NoError(t, err)
	}
	lastLine := func() string {
		out := string(f.do("peek", "", "w-1", "120").Stdout)
		return out[strings.LastIndex(out, "\n")+1:]
	}

	require.Equal(t, "❯ ", lastLine(), "idle: the ready prompt")

	report(3, session.ToAgent, `{"jsonrpc":"2.0","id":7,"method":"session/prompt"}`)
	report(4, session.FromAgent, `{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"working on it"}}}}`)
	require.Contains(t, lastLine(), "esc to interrupt")
	require.Contains(t, string(f.do("peek", "", "w-1", "120").Stdout), "working on it")

	report(5, session.FromAgent, `{"jsonrpc":"2.0","method":"session/request_permission","id":"p1","params":{"toolCall":{"title":"Run ls"}}}`)
	require.Contains(t, lastLine(), "esc to interrupt", "a turn that waits for a permission answer is not idle")

	report(6, session.FromAgent, `{"jsonrpc":"2.0","id":7,"result":{"stopReason":"end_turn"}}`)
	require.Equal(t, "❯ ", lastLine())

	require.Zero(t, f.do("set-meta", "$ ", "w-1", "GC_READY_PROMPT_PREFIX").ExitCode)
	require.Equal(t, "$ ", lastLine(), "the prompt gc published")

	one := string(f.do("peek", "", "w-1", "1").Stdout)
	require.Equal(t, "$ ", one)
	require.Equal(t, int32(66), f.do("peek", "", "nobody", "5").ExitCode, "no box holds the name: no screen")
	require.Equal(t, int32(1), f.do("peek", "", "w-1", "many").ExitCode)
}

func TestThePeekOfASessionBeginsWithTheCommandItWasStartedWith(t *testing.T) {
	f := newFixture(t, operate.Config{})
	startRunning(t, f, "w-1")
	out := string(f.do("peek", "", "w-1", "120").Stdout)
	require.True(t, strings.HasPrefix(out, "$ claude --model sonnet --settings /city/.gc/agents/host/.roomcity/settings.json\n"), out)
}

func TestThePeekOfAProcessIsTheEndOfItsOutput(t *testing.T) {
	m := &procs.Manager{LogDir: filepath.Join(t.TempDir(), "logs"), Grace: 500 * time.Millisecond}
	f := newFixture(t, operate.Config{Processes: m, IsProcess: func(string) bool { return true }})
	cfg := f.startConfig(map[string]any{"command": "echo one; echo two; echo three; sleep 30", "work_dir": f.dir})
	require.Zero(t, f.do("start", cfg, "svc-1").ExitCode)
	require.Eventually(t, func() bool { return strings.Contains(string(f.do("peek", "", "svc-1", "50").Stdout), "three") }, 5*time.Second, 20*time.Millisecond)
	require.Equal(t, "two\nthree", string(f.do("peek", "", "svc-1", "2").Stdout))
	require.Zero(t, f.do("stop", "", "svc-1").ExitCode)
}

func TestLastActivityIsTheTimeOfTheLatestFrame(t *testing.T) {
	f := newFixture(t, operate.Config{})
	require.Empty(t, f.do("get-last-activity", "", "nobody").Stdout)

	startRunning(t, f, "w-1")
	at := time.Date(2026, 11, 16, 7, 8, 9, 0, time.UTC)
	_, err := f.reg.Report(f.ctx, "w-1", []session.HostFrame{{Seq: 3, Time: at, Direction: session.FromAgent, Message: []byte(`{"jsonrpc":"2.0","method":"session/update","params":{}}`)}})
	require.NoError(t, err)
	require.Equal(t, "2026-11-16T07:08:09Z", string(f.do("get-last-activity", "", "w-1").Stdout))
}

func TestInterruptAndTheKeysThatStopATurnCancelIt(t *testing.T) {
	f := newFixture(t, operate.Config{})
	startRunning(t, f, "w-1")
	in, err := f.reg.Instructions(f.ctx, "w-1", 0)
	require.NoError(t, err)

	require.Zero(t, f.do("interrupt", "", "w-1").ExitCode)
	requireInterrupt(t, <-in)
	require.Zero(t, f.do("send-keys", "", "w-1", "Escape").ExitCode)
	requireInterrupt(t, <-in)
	require.Zero(t, f.do("send-keys", "", "w-1", "C-c").ExitCode)
	requireInterrupt(t, <-in)

	// Other keys are dropped, a session that is gone is no failure, and
	// nobody is attached.
	require.Zero(t, f.do("send-keys", "", "w-1", "Enter", "C-u").ExitCode)
	require.Zero(t, f.do("interrupt", "", "nobody").ExitCode)
	require.Zero(t, f.do("clear-scrollback", "", "w-1").ExitCode)
	require.Equal(t, "false", string(f.do("is-attached", "", "w-1").Stdout))
	require.Equal(t, "false", string(f.do("is-attached", "", "nobody").Stdout))
	select {
	case i := <-in:
		t.Fatalf("an instruction no key asked for: %+v", i)
	case <-time.After(100 * time.Millisecond):
	}
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

func TestWhatGcStartsForAServiceAgentRunsAsAProcessOfTheServer(t *testing.T) {
	m := &procs.Manager{LogDir: filepath.Join(t.TempDir(), "logs"), Grace: 500 * time.Millisecond}
	f := newFixture(t, operate.Config{
		Processes: m,
		IsProcess: func(agent string) bool { return strings.HasPrefix(agent, "core.") },
	})
	cfg := f.startConfig(map[string]any{
		"command":       "echo up; sleep 30",
		"work_dir":      f.dir,
		"process_names": []string{"gc"},
		"env":           map[string]string{"GC_AGENT": "core.control-dispatcher", "GC_SESSION_ID": "gc-9"},
	})

	r := f.do("start", cfg, "dispatcher-1")
	require.Zero(t, r.ExitCode, string(r.Stderr))
	require.Empty(t, f.jobs.created, "no Job for a service")
	_, err := f.reg.Get(f.ctx, "dispatcher-1")
	require.ErrorIs(t, err, session.ErrNotFound, "no session either: it has no room")

	require.Equal(t, "true", string(f.do("is-running", "", "dispatcher-1").Stdout))
	require.Equal(t, "true", string(f.do("process-alive", "", "dispatcher-1").Stdout))
	require.Equal(t, "dispatcher-1", string(f.do("list-running", "", "").Stdout))
	require.Equal(t, "gc-9", string(f.do("get-meta", "", "dispatcher-1", "GC_SESSION_ID").Stdout))
	require.Equal(t, "gc", string(f.do("get-meta", "", "dispatcher-1", "GT_PROCESS_NAMES").Stdout))

	r = f.do("start", cfg, "dispatcher-1")
	require.Equal(t, int32(1), r.ExitCode)
	require.Contains(t, string(r.Stderr), "already exists")
	r = f.do("nudge", "wake", "dispatcher-1")
	require.Equal(t, int32(1), r.ExitCode)

	require.Zero(t, f.do("stop", "", "dispatcher-1").ExitCode)
	require.Equal(t, "false", string(f.do("is-running", "", "dispatcher-1").Stdout))
	require.Empty(t, f.do("list-running", "", "").Stdout)
	require.Empty(t, f.do("get-meta", "", "dispatcher-1", "GC_SESSION_ID").Stdout)
	require.Zero(t, f.do("stop", "", "dispatcher-1").ExitCode, "stop is idempotent")
}

func TestAProcessStartSaysWhatItDoesNotCarryOut(t *testing.T) {
	var logged bytes.Buffer
	m := &procs.Manager{LogDir: filepath.Join(t.TempDir(), "logs"), Grace: 500 * time.Millisecond}
	f := newFixture(t, operate.Config{
		Processes: m,
		IsProcess: func(string) bool { return true },
		Log:       slog.New(slog.NewTextHandler(&logged, nil)),
	})
	// The fixture's start config has a nudge, a pre_start and an overlay_dir.
	require.Zero(t, f.do("start", f.startConfig(map[string]any{"command": "sleep 30", "work_dir": filepath.Join(f.dir, "made")}), "svc-1").ExitCode)
	require.Contains(t, logged.String(), "overlay_dir")
	require.Contains(t, logged.String(), "pre_start")
	require.Contains(t, logged.String(), "nudge")
	require.DirExists(t, filepath.Join(f.dir, "made"), "the working directory is made")
	require.Zero(t, f.do("stop", "", "svc-1").ExitCode)
}

func TestAServiceThatEndsOnItsOwnCanBeStartedAgain(t *testing.T) {
	m := &procs.Manager{LogDir: filepath.Join(t.TempDir(), "logs")}
	f := newFixture(t, operate.Config{Processes: m, IsProcess: func(string) bool { return true }})
	cfg := f.startConfig(map[string]any{"command": "exit 1", "work_dir": f.dir})

	require.Zero(t, f.do("start", cfg, "svc-1").ExitCode)
	require.Eventually(t, func() bool { return string(f.do("is-running", "", "svc-1").Stdout) == "false" }, 5*time.Second, 20*time.Millisecond)
	require.Zero(t, f.do("start", cfg, "svc-1").ExitCode)
}

// requireInterrupt checks that i is gc's interrupt: a cancellation that drops
// nothing.
func requireInterrupt(t *testing.T, i session.Instruction) {
	t.Helper()
	require.Equal(t, session.InstructionCancel, i.Kind)
	require.True(t, i.KeepWaiting, "gc's interrupt keeps what waits")
}

func (j *fakeJobs) Info(_ context.Context, name string) (operate.JobInfo, bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.info(name)
}

func (j *fakeJobs) info(name string) (operate.JobInfo, bool, error) {
	alive, held := j.alive[name]
	if !held {
		return operate.JobInfo{}, false, nil
	}
	return operate.JobInfo{Session: name, ID: j.ids[name], Created: time.Unix(1700000000, 0), Alive: alive}, true, nil
}

func (j *fakeJobs) List(context.Context) ([]operate.JobInfo, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []operate.JobInfo
	for name := range j.alive {
		in, _, _ := j.info(name)
		out = append(out, in)
	}
	return out, nil
}

func (j *fakeJobs) DeleteObject(_ context.Context, name, id string) (bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, held := j.alive[name]; !held || j.ids[name] != id {
		return false, nil
	}
	delete(j.alive, name)
	return true, nil
}

// observed is observe's answer.
type observed struct {
	State    string            `json:"state"`
	Object   map[string]string `json:"object"`
	Agent    string            `json:"agent"`
	AgentPID string            `json:"agent_pid"`
	Identity map[string]string `json:"identity"`
}

func observe(t *testing.T, f *fixture, name string, names ...string) observed {
	t.Helper()
	r := f.do("observe", strings.Join(names, "\n"), name)
	require.Zero(t, r.ExitCode, string(r.Stderr))
	var o observed
	require.NoError(t, json.Unmarshal(r.Stdout, &o))
	return o
}

func TestObserveSaysWhatHoldsTheName(t *testing.T) {
	f := newFixture(t, operate.Config{})
	require.Equal(t, "absent", observe(t, f, "w-1").State)

	startRunning(t, f, "w-1")
	o := observe(t, f, "w-1", "claude")
	require.Equal(t, "running", o.State)
	require.Equal(t, map[string]string{"id": "uid-1", "created": "2023-11-14T22:13:20Z"}, o.Object)
	require.Equal(t, "alive", o.Agent, "the agent runs the program the start staged")
	require.Equal(t, map[string]string{"GC_SESSION_ID": "gc-7", "GC_INSTANCE_TOKEN": "", "GT_PROCESS_NAMES": ""}["GC_SESSION_ID"], o.Identity["GC_SESSION_ID"])
	require.Equal(t, o.Object, observe(t, f, "w-1").Object, "the same box is the same object")

	require.Equal(t, "dead", observe(t, f, "w-1", "gc-rpp-no-such-process").Agent)
	require.Equal(t, "unknown", observe(t, f, "w-1").Agent, "no names asked, no verdict")

	// The pod is lost: the Job is held and nothing runs in it.
	f.jobs.mu.Lock()
	f.jobs.alive["w-1"] = false
	f.jobs.mu.Unlock()
	d := observe(t, f, "w-1", "claude")
	require.Equal(t, "dead", d.State)
	require.Equal(t, o.Object, d.Object)
	require.Empty(t, d.Agent)

	require.Zero(t, f.do("stop", "", "w-1").ExitCode)
	require.Equal(t, "absent", observe(t, f, "w-1").State)
}

func TestAFreshOnlyStartOverAHeldNameIsRefusedAndTouchesNothing(t *testing.T) {
	f := newFixture(t, operate.Config{})
	startRunning(t, f, "w-1")
	before := observe(t, f, "w-1")

	fresh := f.startConfig(map[string]any{"fresh_only": true})
	r := f.do("start", fresh, "w-1")
	require.Equal(t, int32(73), r.ExitCode)
	require.Contains(t, string(r.Stderr), "already exists")
	require.Equal(t, before, observe(t, f, "w-1"))

	// A box that is dead holds the name as well.
	f.jobs.mu.Lock()
	f.jobs.alive["w-1"] = false
	f.jobs.mu.Unlock()
	require.Equal(t, int32(73), f.do("start", fresh, "w-1").ExitCode)
	require.Zero(t, f.do("start", f.startConfig(map[string]any{"nudge": ""}), "w-1").ExitCode, "not fresh_only: the dead box is replaced")
	require.NotEqual(t, before.Object["id"], observe(t, f, "w-1").Object["id"], "by another object")
}

func TestInventoryListsEveryBoxWithItsObjectIdentityAndActivity(t *testing.T) {
	f := newFixture(t, operate.Config{})
	var empty struct {
		Complete bool              `json:"complete"`
		Sessions []json.RawMessage `json:"sessions"`
	}
	r := f.do("inventory", "")
	require.Zero(t, r.ExitCode)
	require.NoError(t, json.Unmarshal(r.Stdout, &empty))
	require.True(t, empty.Complete)
	require.Empty(t, empty.Sessions)
	require.JSONEq(t, `{"complete":true,"sessions":[]}`, string(r.Stdout), "an empty city lists [], never null: gc reads null as no list")
	require.Contains(t, string(r.Stdout), `"sessions":[]`)

	startRunning(t, f, "w-1")
	require.Zero(t, f.do("set-meta", "1", "w-1", "GC_RESTART_REQUESTED").ExitCode)
	startRunning(t, f, "w-2")
	f.jobs.mu.Lock()
	f.jobs.alive["w-2"] = false
	f.jobs.mu.Unlock()

	r = f.do("inventory", "")
	require.Zero(t, r.ExitCode, string(r.Stderr))
	var inv struct {
		Complete bool `json:"complete"`
		Sessions []struct {
			Name             string            `json:"name"`
			State            string            `json:"state"`
			Object           map[string]string `json:"object"`
			Attached         *bool             `json:"attached"`
			LastActivity     string            `json:"last_activity"`
			RestartRequested bool              `json:"restart_requested"`
			Identity         map[string]string `json:"identity"`
		} `json:"sessions"`
	}
	require.NoError(t, json.Unmarshal(r.Stdout, &inv))
	require.True(t, inv.Complete)
	require.Len(t, inv.Sessions, 2)
	by := map[string]int{}
	for i, s := range inv.Sessions {
		by[s.Name] = i
	}
	one, two := inv.Sessions[by["w-1"]], inv.Sessions[by["w-2"]]
	require.Equal(t, "running", one.State)
	require.NotEmpty(t, one.LastActivity, "a running session has an activity")
	require.True(t, one.RestartRequested)
	require.False(t, *one.Attached)
	require.Equal(t, "gc-7", one.Identity["GC_SESSION_ID"])
	require.Equal(t, "dead", two.State)
	require.Empty(t, two.LastActivity)
	require.False(t, two.RestartRequested)
}

func TestKillObjectKillsOnlyTheExactObjectAfterItsRecheck(t *testing.T) {
	f := newFixture(t, operate.Config{})
	startRunning(t, f, "w-1")
	cur := observe(t, f, "w-1").Object
	kill := func(id, created, mode string) string {
		body, err := json.Marshal(map[string]any{"object": map[string]string{"id": id, "created": created}, "mode": mode})
		require.NoError(t, err)
		r := f.do("kill-object", string(body), "w-1")
		require.Zero(t, r.ExitCode, string(r.Stderr))
		return string(r.Stdout)
	}

	require.Equal(t, "gone", kill("uid-0", cur["created"], "corpse"), "another object")
	require.Equal(t, "gone", kill(cur["id"], "2001-01-01T00:00:00Z", "zombie"), "created at another time")
	require.Equal(t, "live", kill(cur["id"], cur["created"], "corpse"), "something runs in it")
	require.Equal(t, "changed", kill(cur["id"], cur["created"], "zombie"), "its agent lives as far as the server can tell")
	require.Equal(t, "running", observe(t, f, "w-1").State)

	// The pod is lost: a corpse is killed, and the name is free.
	f.jobs.mu.Lock()
	f.jobs.alive["w-1"] = false
	f.jobs.mu.Unlock()
	require.Equal(t, "killed", kill(cur["id"], cur["created"], "corpse"))
	require.Equal(t, "absent", observe(t, f, "w-1").State)
	require.Equal(t, "gone", kill(cur["id"], cur["created"], "corpse"))

	r := f.do("kill-object", "not json", "w-1")
	require.Equal(t, int32(1), r.ExitCode)
}

func TestAServiceProcessIsABoxWithAPidAndItsAgentIsReadFromTheProcessTable(t *testing.T) {
	if exec.Command("ps", "-ww", "-eo", "pid=,ppid=,args=").Run() != nil {
		t.Skip("the process table cannot be read here")
	}
	m := &procs.Manager{LogDir: filepath.Join(t.TempDir(), "logs"), Grace: 500 * time.Millisecond}
	f := newFixture(t, operate.Config{Processes: m, IsProcess: func(string) bool { return true }})
	require.Zero(t, f.do("start", f.startConfig(map[string]any{"command": "exec sleep 30", "work_dir": f.dir}), "svc-1").ExitCode)

	o := observe(t, f, "svc-1", "sleep")
	require.Equal(t, "running", o.State)
	require.NotEmpty(t, o.AgentPID)
	require.True(t, strings.HasPrefix(o.Object["id"], "p"))
	require.Equal(t, "alive", o.Agent)
	require.Equal(t, "dead", observe(t, f, "svc-1", "gc-rpp-no-such-process").Agent)

	body, err := json.Marshal(map[string]any{"object": o.Object, "mode": "zombie", "agent_pid": o.AgentPID})
	require.NoError(t, err)
	require.Equal(t, "changed", string(f.do("kill-object", string(body), "svc-1").Stdout), "its agent lives")

	require.Zero(t, f.do("stop", "", "svc-1").ExitCode)
	require.Equal(t, "absent", observe(t, f, "svc-1").State)
}

func TestAStartFailsAsSoonAsItsPodEndsWithTheReasonAndTheEndOfItsLog(t *testing.T) {
	f := newFixture(t, operate.Config{StartTimeout: 30 * time.Second, PollEvery: 10 * time.Millisecond})
	f.jobs.failure = "host exited 1 Error\nits log ends:\nFailed to create config directory: Permission denied"
	f.jobs.onCreate = func(spec operate.JobSpec) {
		go func() {
			time.Sleep(50 * time.Millisecond)
			f.jobs.mu.Lock()
			f.jobs.alive[spec.Session] = false
			f.jobs.mu.Unlock()
		}()
	}

	started := time.Now()
	r := f.do("start", f.startConfig(map[string]any{"nudge": ""}), "w-1")
	require.Less(t, time.Since(started), 5*time.Second, "it did not wait out its 30 s")
	require.Equal(t, int32(1), r.ExitCode)
	require.Contains(t, string(r.Stderr), "its pod ended before the agent answered")
	require.Contains(t, string(r.Stderr), "Permission denied")
	require.Contains(t, f.jobs.deleted, "w-1", "the Job is deleted, as for any start that fails")
	require.Equal(t, "false", string(f.do("is-running", "", "w-1").Stdout))
}

func TestAProvidersEnvironmentReachesThePodAndWhatThePodHasOfItsOwnDoesNot(t *testing.T) {
	var logged bytes.Buffer
	f := newFixture(t, operate.Config{Log: slog.New(slog.NewTextHandler(&logged, nil))})

	cfg := f.startConfig(map[string]any{"nudge": "", "env": map[string]string{
		"GC_AGENT": "guest", "GC_SESSION_ID": "gc-8",
		// What city.toml's provider sets for goose.
		"GOOSE_PROVIDER": "openai", "GOOSE_MODEL": "kimi-for-coding", "GOOSE_MODE": "smart_approve", "OPENAI_HOST": "https://api.kimi.com", "OPENAI_BASE_PATH": "coding/v1/chat/completions",
		// What the supervisor's process has, and a pod has of its own.
		"PATH": "/home/linuxbrew/bin", "HOME": "/home/city", "USER": "city", "LOGNAME": "city", "SHELL": "/bin/bash", "PWD": "/home/city/city", "TMPDIR": "/tmp",
		"XDG_RUNTIME_DIR": "/run/user/1000", "KUBERNETES_SERVICE_HOST": "10.0.0.1", "HOSTNAME": "roomcity-0", "TERM": "dumb", "LANG": "en_US.UTF-8", "LANGUAGE": "en_US", "LC_ALL": "en_US.UTF-8", "LC_CTYPE": "en_US.UTF-8",
		// gc asking for a variable to be unset, a name that is no variable, and the server's own.
		"GC_CONTROLLER_TOKEN": "", "not a name": "x", "ROOMCITY_TOKEN": "forged", "ROOMCITY_RUNTIME_URL": "http://127.0.0.1:8082",
		// A known secret.
		"GC_DOLT_PASSWORD": "hunter2", "BEADS_DOLT_PASSWORD": "hunter2-beads",
	}})
	f.jobs.onCreate = func(spec operate.JobSpec) {
		go func() {
			_, err := f.reg.Report(f.ctx, spec.Session, []session.HostFrame{
				{Seq: 1, Direction: session.ToAgent, Message: []byte(`{"jsonrpc":"2.0","id":1,"method":"session/new"}`)},
				{Seq: 2, Direction: session.FromAgent, Message: []byte(`{"jsonrpc":"2.0","id":1,"result":{}}`)},
			})
			require.NoError(t, err)
		}()
	}
	require.Zero(t, f.do("start", cfg, "guest-1").ExitCode)

	require.Len(t, f.jobs.created, 1)
	env := f.jobs.created[0].Env
	for k, v := range map[string]string{
		"GOOSE_PROVIDER": "openai", "GOOSE_MODEL": "kimi-for-coding", "GOOSE_MODE": "smart_approve", "OPENAI_HOST": "https://api.kimi.com",
		"OPENAI_BASE_PATH": "coding/v1/chat/completions", "GC_AGENT": "guest", "GC_SESSION_ID": "gc-8",
		"ROOMCITY_SESSION": "guest-1", "ROOMCITY_SERVER": "http://roomcity.test:8080",
	} {
		require.Equal(t, v, env[k], k)
	}
	for _, k := range []string{"PATH", "HOME", "USER", "LOGNAME", "SHELL", "PWD", "TMPDIR", "XDG_RUNTIME_DIR", "KUBERNETES_SERVICE_HOST", "HOSTNAME", "TERM", "LANG", "LANGUAGE", "LC_ALL", "LC_CTYPE", "GC_CONTROLLER_TOKEN", "not a name", "ROOMCITY_RUNTIME_URL", "GC_DOLT_PASSWORD", "BEADS_DOLT_PASSWORD"} {
		require.NotContains(t, env, k)
	}
	require.NotEqual(t, "forged", env["ROOMCITY_TOKEN"], "what the server sets wins")
	for _, v := range env {
		require.NotEqual(t, "hunter2", v, "a known secret is not in the pod's spec")
		require.NotEqual(t, "hunter2-beads", v)
	}
	require.Contains(t, logged.String(), "GC_DOLT_PASSWORD")
	require.Contains(t, logged.String(), "PATH")
	require.NotContains(t, logged.String(), "hunter2", "the log names the variable, never its value")
}

func TestAStartKeepsTheIdOfGcsSessionBeadForAReset(t *testing.T) {
	f := newFixture(t, operate.Config{})
	startRunning(t, f, "w-1")

	starts, err := f.reg.Starts(f.ctx, "w-1")
	require.NoError(t, err)
	require.Len(t, starts, 1)
	require.Equal(t, "gc-7", starts[0].GCID, "GC_SESSION_ID of the start config")
}

func TestAStartThatFailedAndABoxFoundDeadAreNotRecordedAsStops(t *testing.T) {
	f := newFixture(t, operate.Config{StartTimeout: 150 * time.Millisecond})
	// No pod ever answers: the start fails.
	require.Equal(t, int32(1), f.do("start", f.startConfig(map[string]any{"nudge": ""}), "w-1").ExitCode)
	starts, err := f.reg.Starts(f.ctx, "w-1")
	require.NoError(t, err)
	require.Equal(t, "failed", starts[0].How)

	// A session that runs, whose pod is lost, found dead by gc and killed.
	g := newFixture(t, operate.Config{})
	startRunning(t, g, "w-2")
	cur := observe(t, g, "w-2").Object
	g.jobs.mu.Lock()
	g.jobs.alive["w-2"] = false
	g.jobs.mu.Unlock()
	body, err := json.Marshal(map[string]any{"object": cur, "mode": "corpse"})
	require.NoError(t, err)
	require.Equal(t, "killed", string(g.do("kill-object", string(body), "w-2").Stdout))
	starts, err = g.reg.Starts(g.ctx, "w-2")
	require.NoError(t, err)
	require.Equal(t, "lost", starts[0].How)

	// An ordinary stop is a stop.
	h := newFixture(t, operate.Config{})
	startRunning(t, h, "w-3")
	require.Zero(t, h.do("stop", "", "w-3").ExitCode)
	starts, err = h.reg.Starts(h.ctx, "w-3")
	require.NoError(t, err)
	require.Equal(t, "stop", starts[0].How)
}

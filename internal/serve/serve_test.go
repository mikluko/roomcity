package serve_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	"sarak.sh/mikluko/roomcity/internal/config"
	roomcityv1 "sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1"
	"sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1/roomcityv1connect"
	"sarak.sh/mikluko/roomcity/internal/operate"
	"sarak.sh/mikluko/roomcity/internal/runtimecmd"
	"sarak.sh/mikluko/roomcity/internal/serve"
	"sarak.sh/mikluko/roomcity/internal/sessionauth"
)

// jobs is the Kubernetes side: a Job's pod is a goroutine that runs fn.
type jobs struct {
	mu    sync.Mutex
	alive map[string]bool
	ids   map[string]string
	n     int
	fn    func(operate.JobSpec)
}

func (j *jobs) Create(_ context.Context, s operate.JobSpec) error {
	j.mu.Lock()
	j.alive[s.Session] = true
	j.n++
	if j.ids == nil {
		j.ids = map[string]string{}
	}
	j.ids[s.Session] = fmt.Sprintf("uid-%d", j.n)
	j.mu.Unlock()
	go j.fn(s)
	return nil
}

func (j *jobs) Delete(_ context.Context, name string) error {
	j.mu.Lock()
	delete(j.alive, name)
	j.mu.Unlock()
	return nil
}

func (j *jobs) Failure(context.Context, string) (string, error) { return "", nil }

func (j *jobs) Alive(_ context.Context, name string) (bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.alive[name], nil
}

func (j *jobs) Running(context.Context) ([]string, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []string
	for n := range j.alive {
		out = append(out, n)
	}
	return out, nil
}

func frames(t *testing.T) []*roomcityv1.Frame {
	t.Helper()
	f, err := os.Open("../operate/testdata/counter.acp.jsonl")
	require.NoError(t, err)
	defer f.Close()
	var out []*roomcityv1.Frame
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var line struct {
			Dir string          `json:"dir"`
			Msg json.RawMessage `json:"msg"`
		}
		require.NoError(t, json.Unmarshal(sc.Bytes(), &line))
		d := roomcityv1.FrameDirection_FRAME_DIRECTION_FROM_AGENT
		if line.Dir == "out" {
			d = roomcityv1.FrameDirection_FRAME_DIRECTION_TO_AGENT
		}
		out = append(out, &roomcityv1.Frame{Seq: uint64(len(out) + 1), Direction: d, Message: line.Msg})
	}
	return out
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	return l
}

// pod is what the session host does over the wire: fetch what the start
// staged, attach, and report the recording.
func pod(t *testing.T, ctx context.Context, spec operate.JobSpec, prompts chan<- *roomcityv1.AttachResponse, staged chan<- *roomcityv1.StageManifest) {
	client := roomcityv1connect.NewHostServiceClient(http.DefaultClient, spec.Env["ROOMCITY_SERVER"], sessionauth.Client(spec.Env["ROOMCITY_TOKEN"]))
	stage, err := client.Stage(ctx, connect.NewRequest(&roomcityv1.StageRequest{Session: spec.Session}))
	if err != nil {
		t.Logf("pod: stage: %v", err)
		return
	}
	for stage.Receive() {
		if m := stage.Msg().GetManifest(); m != nil {
			staged <- m
		}
	}
	// Attach returns when the first instruction comes, so it runs alongside
	// the reports, as a host's does.
	go func() {
		att, err := client.Attach(ctx, connect.NewRequest(&roomcityv1.AttachRequest{Session: spec.Session}))
		if err != nil {
			return
		}
		for att.Receive() {
			prompts <- att.Msg()
		}
	}()
	all := frames(t)
	for i := 0; i < len(all); i += 25 {
		_, err := client.Report(ctx, connect.NewRequest(&roomcityv1.ReportRequest{Session: spec.Session, Frames: all[i:min(i+25, len(all))]}))
		if err != nil {
			return
		}
	}
}

func TestAStartAStopAndANudgeThroughTheListeners(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ls := serve.Listeners{Network: listen(t), Runtime: listen(t), Page: listen(t)}
	netURL := "http://" + ls.Network.Addr().String()
	runtimeURL := "http://" + ls.Runtime.Addr().String()

	cfg, err := config.Parse([]byte(`
[server]
data = "` + filepath.Join(t.TempDir(), "data") + `"
city_dir = "` + t.TempDir() + `"
url = "` + netURL + `"

[agents.host]
always_on = true

[kube]
namespace = "sandbox"
image = "roomcity"
`))
	require.NoError(t, err)

	prompts := make(chan *roomcityv1.AttachResponse, 8)
	staged := make(chan *roomcityv1.StageManifest, 1)
	j := &jobs{alive: map[string]bool{}}
	j.fn = func(spec operate.JobSpec) { pod(t, ctx, spec, prompts, staged) }

	served := make(chan error, 1)
	runCtx, stop := context.WithCancel(ctx)
	go func() { served <- serve.Run(runCtx, serve.Options{Config: cfg, Listeners: ls, Jobs: j}) }()

	call := func(stdin string, args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := runtimecmd.Run(ctx, args, []string{runtimecmd.EnvURL + "=" + runtimeURL, "GC_CITY=/city"},
			runtimecmd.Streams{Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errb})
		return code, out.String(), errb.String()
	}

	start, _ := json.Marshal(map[string]any{
		"work_dir": t.TempDir(),
		"command":  "claude --model sonnet",
		"env":      map[string]string{"GC_AGENT": "host"},
		"nudge":    "begin",
	})
	code, _, stderr := call(string(start), "start", "host-1")
	require.Zero(t, code, stderr)

	require.Equal(t, []string{"claude", "--model", "sonnet"}, (<-staged).Command)
	first := <-prompts
	require.Equal(t, "begin", first.GetPrompt().Text)
	require.Equal(t, roomcityv1.PromptSource_PROMPT_SOURCE_GC, first.GetPrompt().Source)

	_, out, _ := call("", "is-running", "host-1")
	require.Equal(t, "true", out)
	code, _, stderr = call("again", "nudge", "host-1")
	require.Zero(t, code, stderr)
	require.Equal(t, "again", (<-prompts).GetPrompt().Text)

	code, _, stderr = call("", "stop", "host-1")
	require.Zero(t, code, stderr)
	require.NotNil(t, (<-prompts).GetStop())
	_, out, _ = call("", "is-running", "host-1")
	require.Equal(t, "false", out)

	// What only the runtime's listener serves is not on the network's, and
	// what a session reaches is not on the runtime's.
	post := func(base, path string) int {
		res, err := http.Post(base+path, "application/json", strings.NewReader("{}"))
		require.NoError(t, err)
		defer res.Body.Close()
		return res.StatusCode
	}
	require.Equal(t, http.StatusNotFound, post(netURL, "/roomcity.v1.RuntimeService/Operate"))
	require.Equal(t, http.StatusNotFound, post(runtimeURL, "/roomcity.v1.HostService/Report"))
	require.Equal(t, http.StatusNotFound, post("http://"+ls.Page.Addr().String(), "/roomcity.v1.RuntimeService/Operate"))

	// The transcript of the session is a page of the page listener alone.
	get := func(base, path string) (int, string) {
		res, err := http.Get(base + path)
		require.NoError(t, err)
		defer func() { _ = res.Body.Close() }()
		var b bytes.Buffer
		_, _ = b.ReadFrom(res.Body)
		return res.StatusCode, b.String()
	}
	status, body := get("http://"+ls.Page.Addr().String(), "/-/host-1")
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, body, "host-1")
	require.Contains(t, body, "ended")
	status, _ = get(netURL, "/-/host-1")
	require.Equal(t, http.StatusNotFound, status, "the sessions' pods do not reach it")
	status, _ = get(runtimeURL, "/-/host-1")
	require.Equal(t, http.StatusNotFound, status)

	stop()
	require.NoError(t, <-served)
}

func (j *jobs) Info(_ context.Context, name string) (operate.JobInfo, bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.info(name)
}

func (j *jobs) info(name string) (operate.JobInfo, bool, error) {
	alive, held := j.alive[name]
	if !held {
		return operate.JobInfo{}, false, nil
	}
	return operate.JobInfo{Session: name, ID: j.ids[name], Created: time.Unix(1700000000, 0), Alive: alive}, true, nil
}

func (j *jobs) List(context.Context) ([]operate.JobInfo, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []operate.JobInfo
	for name := range j.alive {
		in, _, _ := j.info(name)
		out = append(out, in)
	}
	return out, nil
}

func (j *jobs) DeleteObject(_ context.Context, name, id string) (bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, held := j.alive[name]; !held || j.ids[name] != id {
		return false, nil
	}
	delete(j.alive, name)
	return true, nil
}

package host

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	acp "github.com/afrase/acp-go/unstable"
	"github.com/stretchr/testify/require"

	"sarak.sh/mikluko/claudeacp/claudeacp"

	roomcityv1 "sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1"
)

// The tests below run the real goose on Kimi's coding API. They spend its key,
// so they run only when asked, with the key in KIMI_CODE_TOKEN:
//
//	ROOMCITY_REAL_GOOSE=1 KIMI_CODE_TOKEN=... go test ./internal/host -run RealGoose -v
func gooseEnv(t *testing.T) {
	t.Helper()
	if os.Getenv("ROOMCITY_REAL_GOOSE") == "" || os.Getenv("KIMI_CODE_TOKEN") == "" {
		t.Skip("set ROOMCITY_REAL_GOOSE=1 and KIMI_CODE_TOKEN to run the real goose")
	}
	t.Setenv("GOOSE_PROVIDER", "kimi_code")
	t.Setenv("GOOSE_MODEL", "kimi-for-coding")
	t.Setenv("GOOSE_MODE", "smart_approve")
	t.Setenv("GOOSE_TELEMETRY_ENABLED", "false")
}

// gooseHost runs the host on goose acp and returns the server and a stop.
func gooseHost(t *testing.T) (*fakeServer, func()) {
	t.Helper()
	return gooseHostIn(t, t.TempDir())
}

// gooseHostIn is gooseHost with the agent's working directory given.
func gooseHostIn(t *testing.T, dir string) (*fakeServer, func()) {
	t.Helper()
	server, url := newFakeServer(t)
	server.stageWith(&roomcityv1.StageManifest{Command: []string{"goose", "acp"}}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{Session: "s", Token: testToken, Server: url, WorkDir: dir,
			Claude: claudeacp.Config{CLIPath: "/never"}, Retry: time.Millisecond, Log: nil})
	}()
	return server, func() {
		server.instruct(&roomcityv1.AttachResponse{Instruction: &roomcityv1.AttachResponse_Stop{Stop: &roomcityv1.Stop{}}})
		select {
		case <-done:
		case <-time.After(20 * time.Second):
		}
		cancel()
	}
}

func permissionRequest(t *testing.T, fs []*roomcityv1.Frame) (wire, bool) {
	for _, f := range fs {
		if w := decode(t, f); w.Method == "session/request_permission" {
			return w, true
		}
	}
	return wire{}, false
}

// What goose asks when a tool needs approval, and how it ends when the answer
// is "allow once", and when the owner cancels a turn.
func TestRealGooseThroughTheHost(t *testing.T) {
	gooseEnv(t)
	server, stop := gooseHost(t)
	defer stop()

	server.instruct(prompt("Use your shell tool to run exactly `echo roomcity-ok` and then tell me what it printed."))
	var asked wire
	server.eventually(t, "a permission request or the end of the turn", func(fs []*roomcityv1.Frame) bool {
		if w, ok := permissionRequest(t, fs); ok {
			asked = w
			return true
		}
		return len(stopReasons(t, fs)) > 0
	})
	if asked.Method == "" {
		t.Logf("goose asked no permission; frames: %d", len(server.stored()))
	} else {
		t.Logf("permission request, id %s, params %.600s", asked.ID, asked.Params)
		var params struct {
			Options []struct {
				OptionID string `json:"optionId"`
				Kind     string `json:"kind"`
			} `json:"options"`
		}
		require.NoError(t, json.Unmarshal(asked.Params, &params))
		require.NotEmpty(t, params.Options)
		choice := params.Options[0].OptionID
		for _, o := range params.Options {
			if o.Kind == "allow_once" {
				choice = o.OptionID
			}
		}
		server.instruct(&roomcityv1.AttachResponse{Instruction: &roomcityv1.AttachResponse_PermissionAnswer{
			PermissionAnswer: &roomcityv1.PermissionAnswer{RequestId: string(asked.ID), OptionId: choice},
		}})
		server.eventually(t, "the turn's end", func(fs []*roomcityv1.Frame) bool { return len(stopReasons(t, fs)) > 0 })
	}
	t.Logf("stop reasons: %v", stopReasons(t, server.stored()))
	var text strings.Builder
	for _, f := range server.stored() {
		text.Write(f.Message)
	}
	require.Contains(t, text.String(), "roomcity-ok")

	// Cancel a turn that is running a tool.
	before := len(stopReasons(t, server.stored()))
	server.instruct(prompt("Use your shell tool to run exactly `sleep 60` and then say done."))
	var second wire
	server.eventually(t, "the second permission request", func(fs []*roomcityv1.Frame) bool {
		n := 0
		for _, f := range fs {
			if w := decode(t, f); w.Method == "session/request_permission" {
				n++
				second = w
			}
		}
		return n == 2
	})
	server.instruct(&roomcityv1.AttachResponse{Instruction: &roomcityv1.AttachResponse_PermissionAnswer{
		PermissionAnswer: &roomcityv1.PermissionAnswer{RequestId: string(second.ID), OptionId: "allow_once"},
	}})
	time.Sleep(5 * time.Second) // the tool runs
	server.instruct(&roomcityv1.AttachResponse{Instruction: &roomcityv1.AttachResponse_Cancel{Cancel: &roomcityv1.Cancel{}}})
	cancelledAt := time.Now()
	server.eventually(t, "the cancelled turn's end", func(fs []*roomcityv1.Frame) bool { return len(stopReasons(t, fs)) > before })
	t.Logf("cancel ended the turn after %s; stop reasons: %v", time.Since(cancelledAt).Round(time.Millisecond), stopReasons(t, server.stored()))

	// A permission request left open and then cancelled.
	server.instruct(prompt("Use your shell tool to run exactly `echo again`."))
	server.eventually(t, "the third permission request", func(fs []*roomcityv1.Frame) bool {
		n := 0
		for _, f := range fs {
			if decode(t, f).Method == "session/request_permission" {
				n++
			}
		}
		return n == 3
	})
	before = len(stopReasons(t, server.stored()))
	server.instruct(&roomcityv1.AttachResponse{Instruction: &roomcityv1.AttachResponse_Cancel{Cancel: &roomcityv1.Cancel{}}})
	server.eventually(t, "the end of the turn with the request open", func(fs []*roomcityv1.Frame) bool { return len(stopReasons(t, fs)) > before })
	fs := server.stored()
	t.Logf("an open request cancelled: stop reasons %v", stopReasons(t, fs))
	for _, f := range fs {
		if f.Direction == roomcityv1.FrameDirection_FRAME_DIRECTION_TO_AGENT && strings.Contains(string(f.Message), `"outcome"`) {
			t.Logf("answer frame: %.200s", f.Message)
		}
	}
}

// Whether goose takes a second prompt while one runs, and session/load, asked
// of goose directly: the host serializes turns, so only a client of its own can
// ask the first.
func TestRealGooseDirectly(t *testing.T) {
	gooseEnv(t)
	dir := t.TempDir()
	open := func() (*acp.AgentConn, *exec.Cmd, *collector) {
		cmd := exec.Command("goose", "acp")
		cmd.Dir = dir
		cmd.Env = os.Environ()
		in, _ := cmd.StdinPipe()
		out, _ := cmd.StdoutPipe()
		require.NoError(t, cmd.Start())
		c := &collector{}
		return acp.NewAgentConn(out, in, func(*acp.AgentConn) acp.Client { return c }), cmd, c
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	conn, cmd, c := open()
	init, err := conn.Initialize(ctx, &acp.InitializeRequest{ProtocolVersion: acp.LatestProtocolVersion, ClientCapabilities: &acp.ClientCapabilities{}})
	require.NoError(t, err)
	caps, _ := json.Marshal(init.AgentCapabilities)
	t.Logf("agent capabilities: %s", caps)
	sess, err := conn.NewSession(ctx, &acp.NewSessionRequest{Cwd: dir, MCPServers: []acp.MCPServer{}})
	require.NoError(t, err)

	first := make(chan string, 1)
	go func() {
		r, err := conn.Prompt(ctx, &acp.PromptRequest{SessionID: sess.SessionID, Prompt: []acp.ContentBlock{{Text: &acp.TextContent{Text: "Count from 1 to 150 in words, one number per line."}}}})
		if err != nil {
			first <- "error: " + err.Error()
			return
		}
		first <- string(r.StopReason)
	}()
	for c.count() < 5 {
		time.Sleep(50 * time.Millisecond)
	}
	second, err := conn.Prompt(ctx, &acp.PromptRequest{SessionID: sess.SessionID, Prompt: []acp.ContentBlock{{Text: &acp.TextContent{Text: "Say the word ping."}}}})
	if err != nil {
		t.Logf("a second prompt during a running turn: error %v", err)
	} else {
		t.Logf("a second prompt during a running turn: stop reason %s", second.StopReason)
	}
	select {
	case r := <-first:
		t.Logf("the first prompt ended: %s", r)
	case <-time.After(2 * time.Minute):
		t.Log("the first prompt did not end in 2 minutes")
	}

	// session/load in a new process.
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	conn2, cmd2, c2 := open()
	defer func() { _ = cmd2.Process.Kill() }()
	_, err = conn2.Initialize(ctx, &acp.InitializeRequest{ProtocolVersion: acp.LatestProtocolVersion, ClientCapabilities: &acp.ClientCapabilities{}})
	require.NoError(t, err)
	_, err = conn2.LoadSession(ctx, &acp.LoadSessionRequest{SessionID: sess.SessionID, Cwd: dir, MCPServers: []acp.MCPServer{}})
	t.Logf("session/load in a new process: err %v, updates replayed: %d", err, c2.count())
}

// collector is an ACP client that counts the agent's updates and answers any
// permission request with its first option.
type collector struct {
	acp.UnimplementedClient
	mu sync.Mutex
	n  int
}

func (c *collector) SessionUpdate(context.Context, *acp.SessionNotification) error {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	return nil
}

func (c *collector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func (c *collector) RequestPermission(_ context.Context, req *acp.RequestPermissionRequest) (*acp.RequestPermissionResponse, error) {
	return &acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{Selected: &acp.SelectedPermissionOutcome{OptionID: req.Options[0].OptionID}}}, nil
}

// The room's tool attach, called by the real goose: the host answers its
// request for leave itself, and the file reaches the server.
func TestRealGooseAttachesAFileThroughTheRoom(t *testing.T) {
	gooseEnv(t)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "chart.png"), []byte("\x89PNG\r\n\x1a\nrest"), 0o644))
	server, stop := gooseHostIn(t, dir)
	defer stop()

	server.instruct(prompt("Call your attach tool (from the room extension, called room__attach) with path chart.png and caption hi. Do not use any other tool. Then say what it answered."))
	server.eventuallyWithin(t, time.Minute, "the turn's end", func(fs []*roomcityv1.Frame) bool { return len(stopReasons(t, fs)) > 0 })

	request, ok := permissionRequest(t, server.stored())
	require.True(t, ok, "goose asked leave")
	require.Contains(t, string(request.Params), `"answeredBy":"host"`)
	sent := server.sentFiles()
	require.Len(t, sent, 1)
	require.Equal(t, "chart.png", sent[0].Name)
	require.Equal(t, "image/png", sent[0].MediaType)
	require.Equal(t, "hi", sent[0].Caption)
}

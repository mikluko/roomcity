package host

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	acp "github.com/afrase/acp-go/unstable"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"sarak.sh/mikluko/claudeacp/claudeacp"

	roomcityv1 "sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1"
)

// helperAgent is an ACP agent of its own: a prompt asks for permission once,
// then answers "done". It is the test binary run with ROOMCITY_TEST_AGENT set,
// as the host would run any ACP process.
type helperAgent struct {
	acp.UnimplementedAgent
	conn    *acp.ClientConn
	servers []acp.MCPServer
}

func (*helperAgent) Initialize(context.Context, *acp.InitializeRequest) (*acp.InitializeResponse, error) {
	res := &acp.InitializeResponse{ProtocolVersion: acp.LatestProtocolVersion}
	if os.Getenv("ROOMCITY_TEST_AGENT") == "mcp" {
		res.AgentCapabilities = &acp.AgentCapabilities{MCPCapabilities: &acp.MCPCapabilities{HTTP: true}}
	}
	return res, nil
}

func (a *helperAgent) NewSession(_ context.Context, req *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
	if os.Getenv("ROOMCITY_TEST_AGENT") != "mcp" && len(req.MCPServers) > 0 {
		return nil, &acp.Error{Code: acp.ErrorCodeInvalidParams, Message: "an MCP server given to an agent that takes none"}
	}
	a.servers = req.MCPServers
	return &acp.NewSessionResponse{SessionID: "helper-1"}, nil
}

// useRoom is what an agent that takes the room's server does with it: it calls
// the tool attach on the file the prompt names, and says what came of it.
func (a *helperAgent) useRoom(ctx context.Context, req *acp.PromptRequest) (*acp.PromptResponse, error) {
	say := func(text string) {
		_ = a.conn.SessionUpdate(ctx, &acp.SessionNotification{SessionID: req.SessionID, Update: acp.SessionUpdate{
			AgentMessageChunk: &acp.ContentChunk{Content: acp.ContentBlock{Text: &acp.TextContent{Text: text}}},
		}})
	}
	if len(a.servers) != 1 || a.servers[0].HTTP == nil || a.servers[0].HTTP.Name != "room" {
		say("no room server given")
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}
	asked, err := a.conn.RequestPermission(ctx, &acp.RequestPermissionRequest{
		SessionID: req.SessionID,
		ToolCall:  acp.ToolCallUpdate{ToolCallID: "t1", Meta: map[string]any{"claudeCode": map[string]any{"toolName": "mcp__room__attach"}}},
		Options:   []acp.PermissionOption{{OptionID: "yes", Name: "Yes", Kind: acp.PermissionOptionKindAllowOnce}},
	})
	if err != nil || asked.Outcome.Selected == nil || asked.Outcome.Selected.OptionID != "yes" {
		say("leave refused")
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}
	h := a.servers[0].HTTP
	header := map[string]string{}
	for _, kv := range h.Headers {
		header[kv.Name] = kv.Value
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "helper", Version: "1"}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: h.URL, HTTPClient: &http.Client{Transport: bearer{header["Authorization"]}}, DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		say("connect: " + err.Error())
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}
	defer func() { _ = cs.Close() }()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "attach", Arguments: map[string]any{"path": req.Prompt[0].Text.Text, "caption": "from the agent"}})
	if err != nil {
		say("call: " + err.Error())
	} else {
		say("attach: " + res.Content[0].(*mcp.TextContent).Text)
	}
	return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

func (a *helperAgent) Prompt(ctx context.Context, req *acp.PromptRequest) (*acp.PromptResponse, error) {
	if os.Getenv("ROOMCITY_TEST_AGENT") == "mcp" {
		return a.useRoom(ctx, req)
	}
	res, err := a.conn.RequestPermission(ctx, &acp.RequestPermissionRequest{
		SessionID: req.SessionID,
		ToolCall:  acp.ToolCallUpdate{ToolCallID: "t1"},
		Options:   []acp.PermissionOption{{OptionID: "yes", Name: "Yes", Kind: acp.PermissionOptionKindAllowOnce}},
	})
	if err != nil {
		return nil, err
	}
	text := "refused"
	if res.Outcome.Selected != nil {
		text = "done: " + string(res.Outcome.Selected.OptionID)
	}
	_ = a.conn.SessionUpdate(ctx, &acp.SessionNotification{SessionID: req.SessionID, Update: acp.SessionUpdate{
		AgentMessageChunk: &acp.ContentChunk{Content: acp.ContentBlock{Text: &acp.TextContent{Text: text}}},
	}})
	if os.Getenv("ROOMCITY_TEST_AGENT") == "exit" {
		// The answer is written, then the process ends at once.
		go func() { time.Sleep(time.Millisecond); os.Exit(0) }()
	}
	return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

// TestHelperAgent is the process, not a test: it serves the agent on stdio.
func TestHelperAgent(t *testing.T) {
	if os.Getenv("ROOMCITY_TEST_AGENT") == "" {
		t.Skip("the agent process of the tests")
	}
	a := &helperAgent{}
	conn := acp.NewClientConn(os.Stdin, os.Stdout, func(c *acp.ClientConn) acp.Agent { a.conn = c; return a })
	<-conn.Done()
	os.Exit(0)
}

func TestAnAgentThatIsAnACPProcessOfItsOwn(t *testing.T) {
	t.Setenv("ROOMCITY_TEST_AGENT", "1")
	server, url := newFakeServer(t)
	server.stageWith(&roomcityv1.StageManifest{Command: []string{os.Args[0], "-test.run=TestHelperAgent"}}, nil)
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		done <- Run(ctx, Config{Session: "s", Token: testToken, Server: url, WorkDir: t.TempDir(),
			Claude: claudeacp.Config{CLIPath: "/never"}, Retry: time.Millisecond})
	}()

	server.instruct(prompt("do it"))
	var asked wire
	server.eventually(t, "the process's permission request", func(fs []*roomcityv1.Frame) bool {
		for _, f := range fs {
			if w := decode(t, f); w.Method == "session/request_permission" {
				asked = w
				return true
			}
		}
		return false
	})
	server.instruct(&roomcityv1.AttachResponse{Instruction: &roomcityv1.AttachResponse_PermissionAnswer{
		PermissionAnswer: &roomcityv1.PermissionAnswer{RequestId: string(asked.ID), OptionId: "yes"},
	}})
	server.eventually(t, "the turn's end", func(fs []*roomcityv1.Frame) bool { return len(stopReasons(t, fs)) == 1 })
	server.instruct(&roomcityv1.AttachResponse{Instruction: &roomcityv1.AttachResponse_Stop{Stop: &roomcityv1.Stop{}}})

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("the host did not end")
	}
	var text string
	for _, f := range server.stored() {
		text += string(f.Message)
	}
	require.Contains(t, text, "done: yes")
	requireContiguous(t, server.stored())
}

func TestAnAgentThatExitsRightAfterItsAnswerLosesNothing(t *testing.T) {
	t.Setenv("ROOMCITY_TEST_AGENT", "exit")
	for range 10 {
		server, url := newFakeServer(t)
		server.stageWith(&roomcityv1.StageManifest{Command: []string{os.Args[0], "-test.run=TestHelperAgent"}}, nil)
		done := make(chan error, 1)
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			done <- Run(ctx, Config{Session: "s", Token: testToken, Server: url, WorkDir: t.TempDir(),
				Claude: claudeacp.Config{CLIPath: "/never"}, Retry: time.Millisecond})
		}()
		server.instruct(prompt("do it"))
		server.eventually(t, "the permission request", func(fs []*roomcityv1.Frame) bool { _, ok := permissionRequest(t, fs); return ok })
		var asked wire
		asked, _ = permissionRequest(t, server.stored())
		server.instruct(&roomcityv1.AttachResponse{Instruction: &roomcityv1.AttachResponse_PermissionAnswer{
			PermissionAnswer: &roomcityv1.PermissionAnswer{RequestId: string(asked.ID), OptionId: "yes"},
		}})

		// The agent's last frames, the message and the answer to the prompt,
		// arrive although it has gone.
		server.eventually(t, "the turn's end", func(fs []*roomcityv1.Frame) bool { return len(stopReasons(t, fs)) == 1 })
		select {
		case err := <-done:
			require.ErrorContains(t, err, "connection ended")
		case <-time.After(15 * time.Second):
			t.Fatal("the host did not end")
		}
		cancel()
	}
}

// What a process wrote before it ended is still there to be read after it has
// been waited for: the pipes are not closed under the reader.
func TestWhatAProcessWroteBeforeItEndedIsStillReadable(t *testing.T) {
	h := newHost(Config{WorkDir: t.TempDir()})

	ag, err := h.startProcess(context.Background(), []string{"sh", "-c", "printf 'last words\\n'"})
	require.NoError(t, err)
	<-ag.ended // the process has ended and been waited for
	require.NoError(t, ag.err)

	got, err := io.ReadAll(ag.r)
	require.NoError(t, err)
	require.Equal(t, "last words\n", string(got))
}

func TestAnAgentThatEndsAtOnceIsAskedWhyAndSays(t *testing.T) {
	server, url := newFakeServer(t)
	server.stageWith(&roomcityv1.StageManifest{Command: []string{"sh", "-c", "echo 'cannot make the config directory' >&2; exit 3"}}, nil)

	err := Run(context.Background(), Config{Session: "s", Token: testToken, Server: url, WorkDir: t.TempDir(),
		Claude: claudeacp.Config{CLIPath: "/never"}, Retry: time.Millisecond})

	require.ErrorContains(t, err, "connection closed")
	require.ErrorContains(t, err, "exit status 3")
	require.ErrorContains(t, err, "cannot make the config directory")
}

func TestClaudeThatRefusesItsArgumentsSaysSo(t *testing.T) {
	server, url := newFakeServer(t)
	server.stageWith(&roomcityv1.StageManifest{Command: []string{"claude", "--bg"}}, nil)

	err := Run(context.Background(), Config{Session: "s", Token: testToken, Server: url, WorkDir: t.TempDir(),
		Claude: claudeacp.Config{CLIPath: "/never"}, Retry: time.Millisecond})

	require.ErrorContains(t, err, "the agent ended")
	require.ErrorContains(t, err, "--bg")
}

func TestAnAgentThatTakesHttpMCPServersGetsTheRoomAndItsFileReachesTheServer(t *testing.T) {
	t.Setenv("ROOMCITY_TEST_AGENT", "mcp")
	server, url := newFakeServer(t)
	server.stageWith(&roomcityv1.StageManifest{Command: []string{os.Args[0], "-test.run=TestHelperAgent"}}, nil)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "chart.png"), []byte("\x89PNG\r\n\x1a\nrest"), 0o644))
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		done <- Run(ctx, Config{Session: "s", Token: testToken, Server: url, WorkDir: dir,
			Claude: claudeacp.Config{CLIPath: "/never"}, Retry: time.Millisecond})
	}()

	server.instruct(prompt("chart.png"))
	server.eventually(t, "the turn's end", func(fs []*roomcityv1.Frame) bool { return len(stopReasons(t, fs)) == 1 })
	server.instruct(&roomcityv1.AttachResponse{Instruction: &roomcityv1.AttachResponse_Stop{Stop: &roomcityv1.Stop{}}})
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("the host did not end")
	}

	sent := server.sentFiles()
	require.Len(t, sent, 1)
	require.Equal(t, "s", sent[0].Session)
	require.Equal(t, "chart.png", sent[0].Name)
	require.Equal(t, "image/png", sent[0].MediaType)
	require.Equal(t, "from the agent", sent[0].Caption)
	var text string
	for _, f := range server.stored() {
		text += string(f.Message)
	}
	require.Contains(t, text, "chart.png is in the room")

	request, ok := permissionRequest(t, server.stored())
	require.True(t, ok)
	require.Contains(t, string(request.Params), `"answeredBy":"host"`, "the server is told who answered")
}

package host

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	acp "github.com/afrase/acp-go/unstable"
	"github.com/stretchr/testify/require"

	"sarak.sh/mikluko/claudeacp/claudeacp"

	roomcityv1 "sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1"
)

// helperAgent is an ACP agent of its own: a prompt asks for permission once,
// then answers "done". It is the test binary run with ROOMCITY_TEST_AGENT set,
// as the host would run any ACP process.
type helperAgent struct {
	acp.UnimplementedAgent
	conn *acp.ClientConn
}

func (*helperAgent) Initialize(context.Context, *acp.InitializeRequest) (*acp.InitializeResponse, error) {
	return &acp.InitializeResponse{ProtocolVersion: acp.LatestProtocolVersion}, nil
}

func (*helperAgent) NewSession(context.Context, *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
	return &acp.NewSessionResponse{SessionID: "helper-1"}, nil
}

func (a *helperAgent) Prompt(ctx context.Context, req *acp.PromptRequest) (*acp.PromptResponse, error) {
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

package host

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	acp "github.com/afrase/acp-go/unstable"
	"github.com/stretchr/testify/require"

	"sarak.sh/mikluko/claudeacp/claudeacp"
	"sarak.sh/mikluko/claudeacp/claudeagent"

	roomcityv1 "sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1"
)

func TestWithoutSessionFlagsDropsWhatGcNamesTheSessionBy(t *testing.T) {
	got := withoutSessionFlags([]string{"--permission-mode", "acceptEdits", "--session-id", "abc", "--model", "m", "--resume=xyz", "-r", "q", "--settings", "/s.json"})

	require.Equal(t, []string{"--permission-mode", "acceptEdits", "--model", "m", "--settings", "/s.json"}, got)

	// gc's third form, which resumes one session as a fork under another's id.
	got = withoutSessionFlags([]string{"--resume", "P", "--fork-session", "--session-id", "K", "--model", "m"})
	require.Equal(t, []string{"--model", "m"}, got)
}

func TestClaudeStartsWithGcsCommandOnceItsSessionFlagsAreDropped(t *testing.T) {
	server, url := newFakeServer(t)
	server.stageWith(&roomcityv1.StageManifest{Command: []string{"claude", "--permission-mode", "acceptEdits", "--session-id", "a43bbaf5-91d6-450e-993c-5feff382125d", "--model", "claude-haiku-5-5"}}, nil)
	spawned := make(chan claudeagent.Spec, 1)
	f := claude()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{Session: "s", Token: testToken, Server: url, WorkDir: t.TempDir(),
			Claude: claudeacp.Config{CLIPath: "/claude", ConfigDir: t.TempDir(), Spawn: func(ctx context.Context, s claudeagent.Spec) (*claudeagent.Proc, error) {
				spawned <- s
				return f.Spawn(ctx, s)
			}}, Retry: time.Millisecond})
	}()

	server.instruct(prompt("hi"))
	select {
	case spec := <-spawned:
		require.Contains(t, spec.Args, "--model=claude-haiku-5-5")
		require.Contains(t, spec.Args, "--permission-mode=acceptEdits")
		require.NotContains(t, spec.Args, "--session-id=a43bbaf5-91d6-450e-993c-5feff382125d", "gc's own session id is not passed on; claudeacp names the session")
	case err := <-done:
		t.Fatalf("the host ended: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("claude never started")
	}
	cancel()
	<-done
}

func TestClaudeIsGivenItsTaskToolsSoThatPlansComeToTheRoom(t *testing.T) {
	server, url := newFakeServer(t)
	server.stageWith(&roomcityv1.StageManifest{Command: []string{"claude"}}, nil)
	spawned := make(chan claudeagent.Spec, 1)
	f := claude()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{Session: "s", Token: testToken, Server: url, WorkDir: t.TempDir(),
			Claude: claudeacp.Config{CLIPath: "/claude", ConfigDir: t.TempDir(), Spawn: func(ctx context.Context, s claudeagent.Spec) (*claudeagent.Proc, error) {
				spawned <- s
				return f.Spawn(ctx, s)
			}}, Retry: time.Millisecond})
	}()

	server.instruct(prompt("hi"))
	select {
	case spec := <-spawned:
		require.Contains(t, spec.Env, "CLAUDE_CODE_ENABLE_TODO_TOOLS=1")
		require.Contains(t, spec.Env, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1")
		require.Contains(t, spec.Env, "DISABLE_TELEMETRY=1")
		for _, kv := range spec.Env {
			require.NotContains(t, kv, "CLAUDE_CODE_ENABLE_TELEMETRY=", "with no endpoint in the environment there is no export")
		}
	case err := <-done:
		t.Fatalf("the host ended: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("claude never started")
	}
	cancel()
	<-done
}

func TestErrorDataShowsWhatAnACPErrorLeavesOut(t *testing.T) {
	err := fmt.Errorf("session/new: %w", &acp.Error{Code: -32603, Message: "Internal error", Data: json.RawMessage(`{"details":"No provider configured"}`)})

	require.Equal(t, ` (data: {"details":"No provider configured"})`, errorData(err))
	require.Empty(t, errorData(fmt.Errorf("other")))
	require.Empty(t, errorData(&acp.Error{Code: 1, Message: "m"}))
}

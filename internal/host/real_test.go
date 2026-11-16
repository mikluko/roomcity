package host

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"sarak.sh/mikluko/claudeacp/claudeacp"

	roomcityv1 "sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1"
)

// TestRealClaude runs one prompt through the real claude on Haiku. It spends
// the caller's Claude Code login, so it runs only when asked:
//
//	ROOMCITY_REAL_CLAUDE=1 go test ./internal/host -run TestRealClaude -v
func TestRealClaude(t *testing.T) {
	if os.Getenv("ROOMCITY_REAL_CLAUDE") == "" {
		t.Skip("set ROOMCITY_REAL_CLAUDE=1 to run the real claude")
	}
	server, url := newFakeServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{
			Session: "s", Token: testToken, Server: url, WorkDir: t.TempDir(),
			Claude: claudeacp.Config{ClaudeArgs: []string{"--model", "claude-haiku-5-5"}},
			Retry:  time.Millisecond,
		})
	}()

	server.instruct(prompt("Reply with the single word: pong"))
	server.eventually(t, "the answer", func(fs []*roomcityv1.Frame) bool { return len(stopReasons(t, fs)) == 1 })
	server.instruct(&roomcityv1.AttachResponse{Instruction: &roomcityv1.AttachResponse_Stop{Stop: &roomcityv1.Stop{}}})
	require.NoError(t, <-done)

	frames := server.stored()
	requireContiguous(t, frames)
	var text strings.Builder
	for _, f := range frames {
		text.Write(f.Message)
		w := decode(t, f)
		t.Logf("%3d %-10s %s", f.Seq, f.Direction.String()[len("FRAME_DIRECTION_"):], w.Method)
	}
	require.Equal(t, []string{"end_turn"}, stopReasons(t, frames))
	require.Contains(t, strings.ToLower(text.String()), "pong")
}

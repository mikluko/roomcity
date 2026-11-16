package serve

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"sarak.sh/mikluko/roomcity/internal/session"
)

func TestTranscriptsPastTheirTermAreRemovedNowAndThen(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	reg, err := session.Open(ctx, filepath.Join(t.TempDir(), "r.db"))
	require.NoError(t, err)
	defer func() { _ = reg.Close() }()
	_, err = reg.Start(ctx, "old", "worker", session.Task)
	require.NoError(t, err)
	require.NoError(t, reg.End(ctx, "old"))
	_, err = reg.Start(ctx, "running", "host", session.AlwaysOn)
	require.NoError(t, err)

	done := make(chan struct{})
	loopCtx, stop := context.WithCancel(ctx)
	go func() {
		// A negative term puts the cut-off in the future, past every end.
		purgeTranscripts(loopCtx, reg, -time.Hour, 10*time.Millisecond, slog.New(slog.DiscardHandler))
		close(done)
	}()
	require.Eventually(t, func() bool {
		_, _, err := reg.Transcript(ctx, "old")
		return err == session.ErrNotFound
	}, 5*time.Second, 10*time.Millisecond)
	_, _, err = reg.Transcript(ctx, "running")
	require.NoError(t, err, "a session still running is never removed")

	stop()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop did not end with its context")
	}
}

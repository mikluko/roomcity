package session

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// threeStarts makes a session that was started, stopped, started afresh,
// stopped and started a third time, at the times a clock gives.
func threeStarts(t *testing.T) (context.Context, *Registry, time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	r, err := Open(ctx, filepath.Join(t.TempDir(), "r.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	t0 := time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)
	at := t0
	r.now = func() time.Time { return at }

	for i := 0; i < 3; i++ {
		at = t0.Add(time.Duration(i) * 2 * time.Hour)
		_, err = r.StartWith(ctx, "s", "host", AlwaysOn, StartInfo{GCID: "gc-" + string(rune('a'+i))})
		require.NoError(t, err)
		require.NoError(t, r.SetRoom(ctx, "s", "!room"))
		if i < 2 {
			_, err = r.Report(ctx, "s", []HostFrame{{Seq: 1, Direction: FromAgent, Message: []byte(`{"n":1}`)}})
			require.NoError(t, err)
			at = at.Add(time.Minute)
			require.NoError(t, r.End(ctx, "s"))
		}
	}
	return ctx, r, t0
}

func TestTranscriptIsOfOneStart(t *testing.T) {
	ctx, r, _ := threeStarts(t)

	card, entries, err := r.Transcript(ctx, "s", 1)
	require.NoError(t, err)
	require.Equal(t, 1, card.Start.N)
	require.Equal(t, "stop", card.Start.How)
	require.Equal(t, "!room", card.Start.Room)
	require.Equal(t, "gc-a", card.Start.GCID)
	require.Len(t, card.Starts, 3, "the card knows the others, for the links between them")
	require.Len(t, entries, 3, "started, a frame, stopped: nothing of the later starts")
	require.Equal(t, Started, entries[0].Event.(Lifecycle).Kind)
	require.Equal(t, Stopped, entries[2].Event.(Lifecycle).Kind)

	card, entries, err = r.Transcript(ctx, "s", 0)
	require.NoError(t, err)
	require.Equal(t, 3, card.Start.N, "none is the latest")
	require.Equal(t, "", card.Start.How, "which is open")
	require.True(t, card.Ended.IsZero())
	require.Len(t, entries, 1)
	require.Equal(t, StartedAfresh, entries[0].Event.(Lifecycle).Kind)

	_, _, err = r.Transcript(ctx, "s", 4)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestPurgeCountsTheTermOfEachStartFromItsOwnEnd(t *testing.T) {
	ctx, r, t0 := threeStarts(t)

	// Start 1 ended at 08:01, start 2 at 10:01, start 3 is open.
	n, err := r.Purge(ctx, t0.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, n, "the first start is past its term and the second is not")
	_, _, err = r.Transcript(ctx, "s", 1)
	require.ErrorIs(t, err, ErrNotFound)
	_, entries, err := r.Transcript(ctx, "s", 2)
	require.NoError(t, err)
	require.Len(t, entries, 3)

	n, err = r.Purge(ctx, t0.Add(3*time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, n)
	_, _, err = r.Transcript(ctx, "s", 2)
	require.ErrorIs(t, err, ErrNotFound)

	card, _, err := r.Transcript(ctx, "s", 0)
	require.NoError(t, err, "the open start stays")
	require.Equal(t, 3, card.Start.N)
	n, err = r.Purge(ctx, t0.Add(100*time.Hour))
	require.NoError(t, err)
	require.Zero(t, n, "a session that has not ended is never removed")

	_, err = r.StartWith(ctx, "s", "host", AlwaysOn, StartInfo{})
	require.NoError(t, err)
	card, entries, err = r.Transcript(ctx, "s", 0)
	require.NoError(t, err)
	require.Equal(t, 4, card.Start.N)
	require.Greater(t, entries[0].Event.Number(), uint64(7), "the numbering of events goes on past what was removed")
}

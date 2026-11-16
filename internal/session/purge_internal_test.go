package session

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPurgeOfASessionStartedAgainSinceItWasChosenTouchesNothing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r, err := Open(ctx, filepath.Join(t.TempDir(), "r.db"))
	require.NoError(t, err)
	defer func() { _ = r.Close() }()

	_, err = r.Start(ctx, "s", "worker", Task)
	require.NoError(t, err)
	require.NoError(t, r.SetMark(ctx, "s", "k", "v"))
	require.NoError(t, r.End(ctx, "s"))
	later := time.Now().Add(time.Hour)

	// Purge chose "s" when it had ended; gc starts the name again before the
	// removal gets to it.
	_, err = r.Start(ctx, "s", "worker", Task)
	require.NoError(t, err)
	require.NoError(t, r.PutBundle(ctx, "s", []byte("m"), []byte("a")))

	gone, err := r.purgeOne(ctx, "s", later)
	require.NoError(t, err)
	require.Zero(t, gone, "a session that is not ended is not removed")

	card, entries, err := r.Transcript(ctx, "s", 0)
	require.NoError(t, err)
	require.Equal(t, Starting, card.State)
	require.NotEmpty(t, entries, "its events are still there")
	mark, err := r.Mark(ctx, "s", "k")
	require.NoError(t, err)
	require.Equal(t, "v", mark, "and its marks")
	_, _, err = r.Bundle(ctx, "s")
	require.NoError(t, err, "and its bundle")
}

func TestPurgeLeavesASessionThatEndedAgainJustNow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r, err := Open(ctx, filepath.Join(t.TempDir(), "r.db"))
	require.NoError(t, err)
	defer func() { _ = r.Close() }()
	_, err = r.Start(ctx, "s", "worker", Task)
	require.NoError(t, err)
	require.NoError(t, r.End(ctx, "s"))
	cutoff := time.Now().Add(-time.Hour)

	gone, err := r.purgeOne(ctx, "s", cutoff)
	require.NoError(t, err)
	require.Zero(t, gone, "it ended after the cut-off")
	gone, err = r.purgeOne(ctx, "s", time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, gone)
}

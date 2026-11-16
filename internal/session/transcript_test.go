package session_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"sarak.sh/mikluko/roomcity/internal/session"
)

func finishedSession(t *testing.T, r *session.Registry, name string) {
	t.Helper()
	ctx := ctxT(t)
	_, err := r.Start(ctx, name, "worker", session.Task)
	require.NoError(t, err)
	_, err = r.Prompt(ctx, name, session.PromptInput{Text: "do it"})
	require.NoError(t, err)
	_, err = r.Report(ctx, name, []session.HostFrame{
		{Seq: 1, Direction: session.ToAgent, Message: []byte(`{"method":"session/prompt"}`)},
		{Seq: 2, Direction: session.FromAgent, Message: []byte(`{"result":{"stopReason":"end_turn"}}`)},
	})
	require.NoError(t, err)
	require.NoError(t, r.SetMark(ctx, name, "k", "v"))
	require.NoError(t, r.End(ctx, name))
}

func TestTranscriptHoldsTheWholeStreamWithItsTimesAndTheCard(t *testing.T) {
	ctx := ctxT(t)
	r := open(t, filepath.Join(t.TempDir(), "r.db"))
	before := time.Now()
	finishedSession(t, r, "s")

	card, entries, err := r.Transcript(ctx, "s")
	require.NoError(t, err)
	require.Equal(t, session.Info{Name: "s", Agent: "worker", Kind: session.Task, State: session.Ended}, card.Info)
	require.False(t, card.Began.Before(before.Add(-time.Second)))
	require.False(t, card.Ended.Before(card.Began))

	var kinds []string
	for i, e := range entries {
		require.Equal(t, uint64(i+1), e.Event.(interface{ Number() uint64 }).Number(), "in order, from 1")
		require.False(t, e.Time.IsZero())
		switch ev := e.Event.(type) {
		case session.Lifecycle:
			kinds = append(kinds, "lifecycle")
			_ = ev
		case session.Prompt:
			kinds = append(kinds, "prompt")
		case session.Frame:
			kinds = append(kinds, "frame")
			require.Equal(t, e.Time, ev.Time)
		}
	}
	require.Equal(t, []string{"lifecycle", "prompt", "frame", "frame", "lifecycle"}, kinds)

	_, _, err = r.Transcript(ctx, "nobody")
	require.ErrorIs(t, err, session.ErrNotFound)
}

func TestPurgeRemovesWhatEndedBeforeTheTimeAndNothingElse(t *testing.T) {
	ctx := ctxT(t)
	r := open(t, filepath.Join(t.TempDir(), "r.db"))
	finishedSession(t, r, "old")
	require.NoError(t, r.PutBundle(ctx, "old", []byte("m"), []byte("a")))
	_, err := r.Start(ctx, "running", "host", session.AlwaysOn)
	require.NoError(t, err)

	n, err := r.Purge(ctx, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.Zero(t, n, "nothing ended before an hour ago")
	_, _, err = r.Transcript(ctx, "old")
	require.NoError(t, err)

	n, err = r.Purge(ctx, time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, n, "the ended session goes, the running one stays")
	_, _, err = r.Transcript(ctx, "old")
	require.ErrorIs(t, err, session.ErrNotFound)
	_, _, err = r.Transcript(ctx, "running")
	require.NoError(t, err)

	mark, err := r.Mark(ctx, "old", "k")
	require.NoError(t, err)
	require.Empty(t, mark, "its marks went with it")
	_, _, err = r.Bundle(ctx, "old")
	require.ErrorIs(t, err, session.ErrNotFound, "and its bundle")

	_, err = r.Start(ctx, "old", "worker", session.Task)
	require.NoError(t, err, "a name that was purged can be used again")
	evs, err := r.Subscribe(ctx, "old", 0)
	require.NoError(t, err)
	require.Equal(t, session.Lifecycle{Seq: 1, Kind: session.Started}, recv(t, evs), "and begins from 1")
}

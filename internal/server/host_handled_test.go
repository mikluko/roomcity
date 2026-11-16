package server_test

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	roomcityv1 "sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1"
	"sarak.sh/mikluko/roomcity/internal/session"
)

func TestHostHandsOverAnOwnersPromptAsOneThatGoesIntoTheTurn(t *testing.T) {
	f := newHostFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := f.reg.Nudge(ctx, "s", "from gc")
	require.NoError(t, err)
	_, err = f.reg.Prompt(ctx, "s", session.PromptInput{Text: "from owner"})
	require.NoError(t, err)

	stream, err := f.client.Attach(ctx, connect.NewRequest(&roomcityv1.AttachRequest{Session: "s"}))
	require.NoError(t, err)
	require.True(t, stream.Receive())
	require.False(t, stream.Msg().GetPrompt().IntoTurn, "gc's nudge waits for the turn's end")
	require.True(t, stream.Receive())
	require.True(t, stream.Msg().GetPrompt().IntoTurn, "the owner's goes into the running turn where it can")
}

func TestHostHandledWritesWhatBecameOfAPromptOnceAndInOrder(t *testing.T) {
	f := newHostFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stored, err := f.reg.Prompt(ctx, "s", session.PromptInput{Text: "steer"})
	require.NoError(t, err)
	events, err := f.reg.Subscribe(ctx, "s", session.FromNow)
	require.NoError(t, err)

	handled := func(h roomcityv1.Handling) error {
		_, err := f.client.Handled(ctx, connect.NewRequest(&roomcityv1.HandledRequest{Session: "s", InstructionSeq: 1, Handling: h}))
		return err
	}
	require.NoError(t, handled(roomcityv1.Handling_HANDLING_QUEUED))
	require.NoError(t, handled(roomcityv1.Handling_HANDLING_QUEUED))
	require.NoError(t, handled(roomcityv1.Handling_HANDLING_STARTED))
	require.NoError(t, handled(roomcityv1.Handling_HANDLING_STARTED))

	untimed := func(e session.Event) session.Event {
		p := e.(session.Prompt)
		p.Time = time.Time{}
		return p
	}
	waiting := untimed(<-events)
	delivered := untimed(<-events)
	require.Equal(t, session.Prompt{Seq: stored + 1, Source: session.FromOwner, Text: "steer", State: session.Waiting, Of: stored}, waiting)
	require.Equal(t, session.Prompt{Seq: stored + 2, Source: session.FromOwner, Text: "steer", State: session.Delivered, Of: stored}, delivered)
}

func TestHostHandledRefusesWhatItCannotPlace(t *testing.T) {
	f := newHostFixture(t)
	ctx := context.Background()

	_, err := f.anon.Handled(ctx, connect.NewRequest(&roomcityv1.HandledRequest{Session: "s", InstructionSeq: 1, Handling: roomcityv1.Handling_HANDLING_STARTED}))
	require.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err))

	_, err = f.client.Handled(ctx, connect.NewRequest(&roomcityv1.HandledRequest{Session: "s", InstructionSeq: 1}))
	require.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err), "no handling is a mistake")

	_, err = f.client.Handled(ctx, connect.NewRequest(&roomcityv1.HandledRequest{Session: "s", InstructionSeq: 9, Handling: roomcityv1.Handling_HANDLING_STARTED}))
	require.Equal(t, connect.CodeNotFound, connect.CodeOf(err), "no such instruction")
}

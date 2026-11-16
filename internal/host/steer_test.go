package host

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"sarak.sh/mikluko/claudeacp/claudeagent/claudetest"

	roomcityv1 "sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1"
)

func intoTurn(text string) *roomcityv1.AttachResponse {
	in := prompt(text)
	in.GetPrompt().IntoTurn = true
	return in
}

func gcNudge(text string) *roomcityv1.AttachResponse {
	in := prompt(text)
	in.GetPrompt().Source = roomcityv1.PromptSource_PROMPT_SOURCE_GC
	return in
}

func (s *fakeServer) waitHandled(t *testing.T, n int) []handledCall {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for len(s.handlings()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("the host told the server of %d prompts, not %d: %v", len(s.handlings()), n, s.handlings())
		}
		time.Sleep(time.Millisecond)
	}
	return s.handlings()
}

func TestAnOwnersPromptGoesIntoTheRunningTurnWhereTheAgentTakesIt(t *testing.T) {
	started, steered := make(chan struct{}), make(chan struct{})
	r := start(t, claude(
		claudetest.Expect(`{"type":"user"}`), claudetest.Send(echo), claudetest.Send(running),
		claudetest.Do(func() { close(started) }),
		claudetest.Expect(`{"type":"user","priority":"now"}`), claudetest.Do(func() { close(steered) }),
		claudetest.Send(result), claudetest.Send(idle),
	))
	r.server.instruct(prompt("running"))
	<-started
	r.server.instruct(intoTurn("also this"))
	<-steered
	r.server.eventually(t, "the turn ended", func(fs []*roomcityv1.Frame) bool { return len(stopReasons(t, fs)) == 1 })
	r.stop(t)

	require.Equal(t, []handledCall{
		{1, roomcityv1.Handling_HANDLING_STARTED},
		{2, roomcityv1.Handling_HANDLING_STEERED},
	}, r.server.handlings())
	var prompts, steers int
	for _, m := range methods(t, r.server.stored(), roomcityv1.FrameDirection_FRAME_DIRECTION_TO_AGENT) {
		switch m {
		case "session/prompt":
			prompts++
		case "_session/steering":
			steers++
		}
	}
	require.Equal(t, 1, prompts, "the steered text is no prompt of its own")
	require.Equal(t, 1, steers)
	require.Len(t, stopReasons(t, r.server.stored()), 1)
}

func TestAPromptThatGoesAtOnceIsReportedStartedAndNeverQueued(t *testing.T) {
	r := start(t, claude(answering("hi")...))
	r.server.instruct(intoTurn("hello"))
	r.server.eventually(t, "the turn ended", func(fs []*roomcityv1.Frame) bool { return len(stopReasons(t, fs)) == 1 })
	r.stop(t)
	require.Equal(t, []handledCall{{1, roomcityv1.Handling_HANDLING_STARTED}}, r.server.handlings())
}

func TestAPromptThatMustWaitIsReportedQueuedAndThenStarted(t *testing.T) {
	release, started := make(chan struct{}), make(chan struct{})
	steps := []claudetest.Step{
		claudetest.Expect(`{"type":"user"}`), claudetest.Send(echo), claudetest.Send(running),
		claudetest.Do(func() { close(started) }), claudetest.Until(release),
		claudetest.Send(result), claudetest.Send(idle),
	}
	steps = append(steps, answering("later")...)
	r := start(t, claude(steps...))
	r.server.instruct(prompt("running"))
	<-started
	r.server.instruct(gcNudge("wake up"))
	require.Equal(t, []handledCall{
		{1, roomcityv1.Handling_HANDLING_STARTED},
		{2, roomcityv1.Handling_HANDLING_QUEUED},
	}, r.server.waitHandled(t, 2), "gc's nudge waits for the turn's end, whatever the agent takes")

	close(release)
	r.server.eventually(t, "two turns", func(fs []*roomcityv1.Frame) bool { return len(stopReasons(t, fs)) == 2 })
	r.stop(t)
	require.Equal(t, []handledCall{
		{1, roomcityv1.Handling_HANDLING_STARTED},
		{2, roomcityv1.Handling_HANDLING_QUEUED},
		{2, roomcityv1.Handling_HANDLING_STARTED},
	}, r.server.handlings())
}

func TestWhatBecameOfAPromptIsToldAgainUntilTheServerTakesIt(t *testing.T) {
	r := start(t, claude(answering("a")...))
	r.server.mu.Lock()
	r.server.failHandled = 3
	r.server.mu.Unlock()
	r.server.instruct(prompt("hello"))
	require.Equal(t, []handledCall{{1, roomcityv1.Handling_HANDLING_STARTED}}, r.server.waitHandled(t, 1))
	r.stop(t)
}

func TestAHostWhoseAgentTakesNoMessageIntoATurnNeverSteers(t *testing.T) {
	h := &host{steering: false, turning: true, wake: make(chan struct{}, 1)}
	require.False(t, h.steered(t.Context(), 1, "text"))
	require.Empty(t, h.reports, "nothing was done, so nothing is reported")
}

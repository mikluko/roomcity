package chat_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"sarak.sh/mikluko/roomcity/internal/chat"
	"sarak.sh/mikluko/roomcity/internal/chat/chattest"
	"sarak.sh/mikluko/roomcity/internal/session"
)

func gcPrompt(text string, at time.Time) session.Prompt {
	return session.Prompt{Source: session.FromGC, Text: text, State: session.Stored, Time: at}
}

func agentChunk(text string, at time.Time) session.Frame {
	msg, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "method": "session/update",
		"params": map[string]any{"update": map[string]any{
			"sessionUpdate": "agent_message_chunk",
			"content":       map[string]any{"type": "text", "text": text},
		}},
	})
	return session.Frame{Direction: session.FromAgent, Message: msg, Time: at}
}

func endOfTurn(at time.Time) session.Frame {
	return session.Frame{Direction: session.FromAgent, Time: at, Message: []byte(`{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn"}}`)}
}

func bodiesIn(d *fakeDriver, room chat.RoomID) []string {
	var out []string
	for _, m := range d.messagesIn(room) {
		out = append(out, m.Msg.Body)
	}
	return out
}

func TestServiceShowsANewSessionFromItsFirstEvent(t *testing.T) {
	now := time.Now()
	sessions := &chattest.Sessions{}
	// The role prompt is written to the stream before anyone tells the chat
	// side that the session exists.
	sessions.Emit("s-host", session.Lifecycle{Kind: session.Started, Time: now}, gcPrompt("you are host", now))
	sessions.Add(alwaysOn("host", session.Starting))
	driver, _ := runService(t, sessions)

	require.Eventually(t, func() bool { return len(bodiesIn(driver, "!host")) == 1 }, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, "From gc: you are host", bodiesIn(driver, "!host")[0])
}

func TestServiceShowsNothingOfASessionThatWasShownBeforeARestart(t *testing.T) {
	now := time.Now()
	sessions := &chattest.Sessions{}
	sessions.Emit("s-host", gcPrompt("shown before", now), gcPrompt("not shown yet", now))
	info := alwaysOn("host", session.Running)
	info.Shown = 1
	sessions.Add(info)
	driver, _ := runService(t, sessions)

	require.Eventually(t, func() bool { return len(bodiesIn(driver, "!host")) == 1 }, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, "From gc: not shown yet", bodiesIn(driver, "!host")[0])
	require.Never(t, func() bool { return len(bodiesIn(driver, "!host")) > 1 }, 400*time.Millisecond, 20*time.Millisecond)
}

func TestServiceRecordsHowFarASessionIsShownAtTheLastSettledEvent(t *testing.T) {
	now := time.Now()
	sessions := &chattest.Sessions{}
	sessions.Add(alwaysOn("host", session.Running))
	driver, _ := runService(t, sessions)
	require.Eventually(t, func() bool { return len(driver.roomSpecs()) == 1 }, 5*time.Second, 10*time.Millisecond)

	sessions.Emit("s-host", agentChunk("first paragraph\n\nsecond, still", now))
	require.Eventually(t, func() bool { return len(bodiesIn(driver, "!host")) == 1 }, 5*time.Second, 10*time.Millisecond)
	require.Never(t, func() bool { return sessions.ShownSeq("s-host") > 0 }, 600*time.Millisecond, 20*time.Millisecond,
		"the second paragraph is held back: a restart has to show it, so the record stays behind it")

	sessions.Emit("s-host", agentChunk(" going", now), endOfTurn(now))
	require.Eventually(t, func() bool { return sessions.ShownSeq("s-host") == 3 }, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, []string{"first paragraph", "second, still going"}, bodiesIn(driver, "!host"))
}

func TestServiceShowsWhatTheSessionSaidWhileTheHomeserverWasDown(t *testing.T) {
	now := time.Now()
	sessions := &chattest.Sessions{}
	sessions.Add(alwaysOn("host", session.Running))
	driver, _ := runService(t, sessions)
	require.Eventually(t, func() bool { return len(driver.roomSpecs()) == 1 }, 5*time.Second, 10*time.Millisecond)

	driver.setDown(true)
	sessions.Emit("s-host", agentChunk("said while it was down\n\n", now))
	require.Never(t, func() bool { return sessions.ShownSeq("s-host") > 0 }, 600*time.Millisecond, 20*time.Millisecond,
		"what could not be sent is not counted as shown")
	require.Empty(t, bodiesIn(driver, "!host"))

	driver.setDown(false)
	require.Eventually(t, func() bool { return len(bodiesIn(driver, "!host")) == 1 }, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, "said while it was down", bodiesIn(driver, "!host")[0])

	sessions.Emit("s-host", endOfTurn(now))
	require.Eventually(t, func() bool { return sessions.ShownSeq("s-host") == 2 }, 5*time.Second, 10*time.Millisecond)
	require.Len(t, bodiesIn(driver, "!host"), 1, "and it is shown once")
}

func TestServiceLeavesOutWhatIsOlderThanTheCatchUpAndSaysSo(t *testing.T) {
	now := time.Now()
	sessions := &chattest.Sessions{}
	sessions.Emit("s-host",
		gcPrompt("three hours ago", now.Add(-3*time.Hour)),
		gcPrompt("two hours ago", now.Add(-2*time.Hour)),
		gcPrompt("just now", now))
	sessions.Add(alwaysOn("host", session.Running))
	driver, _ := runServiceWith(t, sessions, newFakeDriver(), func(s *chat.Service) { s.CatchUp = time.Hour })

	require.Eventually(t, func() bool { return len(bodiesIn(driver, "!host")) == 2 }, 5*time.Second, 10*time.Millisecond)
	got := bodiesIn(driver, "!host")
	require.Contains(t, got[0], "is not shown here")
	require.Contains(t, got[0], "1h0m0s")
	require.Contains(t, got[0], "https://t/-/s-host", "the notice links the transcript")
	require.Equal(t, "From gc: just now", got[1])
	require.Eventually(t, func() bool { return sessions.ShownSeq("s-host") == 3 }, 5*time.Second, 10*time.Millisecond)
}

func TestServiceSaysSoWhenOnlyOldEventsAreLeft(t *testing.T) {
	now := time.Now()
	sessions := &chattest.Sessions{}
	sessions.Emit("s-host", gcPrompt("long ago", now.Add(-5*time.Hour)))
	sessions.Add(alwaysOn("host", session.Running))
	driver, _ := runServiceWith(t, sessions, newFakeDriver(), func(s *chat.Service) { s.CatchUp = time.Hour })

	require.Eventually(t, func() bool { return len(bodiesIn(driver, "!host")) == 1 }, 5*time.Second, 10*time.Millisecond)
	require.True(t, strings.Contains(bodiesIn(driver, "!host")[0], "is not shown here"))
	require.Eventually(t, func() bool { return sessions.ShownSeq("s-host") == 1 }, 5*time.Second, 10*time.Millisecond)
	require.Never(t, func() bool { return len(bodiesIn(driver, "!host")) > 1 }, 600*time.Millisecond, 20*time.Millisecond,
		"once, not on every tick")
}

func TestServiceLeavesOutWhatTheHomeserverRefusesAndGoesOn(t *testing.T) {
	now := time.Now()
	sessions := &chattest.Sessions{}
	sessions.Add(alwaysOn("host", session.Running))
	driver := newFakeDriver()
	driver.refuse = "refused one"
	driver, _ = runServiceWith(t, sessions, driver)
	require.Eventually(t, func() bool { return len(driver.roomSpecs()) == 1 }, 5*time.Second, 10*time.Millisecond)

	sessions.Emit("s-host", agentChunk("refused one\n\nkept two\n\n", now), endOfTurn(now))
	require.Eventually(t, func() bool { return len(bodiesIn(driver, "!host")) == 1 }, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, []string{"kept two"}, bodiesIn(driver, "!host"))
	require.Eventually(t, func() bool { return sessions.ShownSeq("s-host") == 2 }, 5*time.Second, 10*time.Millisecond,
		"what was refused counts as shown")
	require.Equal(t, 1, sessions.Subscriptions("s-host"), "and the stream is not read again: a refusal is not an outage")
	require.Never(t, func() bool { return len(bodiesIn(driver, "!host")) > 1 }, 500*time.Millisecond, 20*time.Millisecond)
}

func TestServiceTellsTheJournalOfAStartOnlyOnceTheRoomHasTheEvent(t *testing.T) {
	sessions := &chattest.Sessions{}
	sessions.SetStarts("s-host", session.Start{N: 1, FirstSeq: 1, LastSeq: 6}, session.Start{N: 2, FirstSeq: 7})
	sessions.Add(alwaysOn("host", session.Running))
	driver := newFakeDriver()
	driver.setFailBody("The session began anew; nothing of the earlier one is remembered.")
	driver, _ = runServiceWith(t, sessions, driver)
	require.Eventually(t, func() bool { return len(driver.roomSpecs()) == 1 }, 5*time.Second, 10*time.Millisecond)

	sessions.Emit("s-host", session.Lifecycle{Seq: 7, Kind: session.StartedAfresh, Time: time.Now()})
	require.Never(t, func() bool { return len(driver.messagesIn("!journal")) > 0 }, 700*time.Millisecond, 20*time.Millisecond,
		"the room's own notice is not taken, and is tried again: the journal is not told at each try")

	driver.setFailBody("")
	require.Eventually(t, func() bool { return len(driver.messagesIn("!journal")) == 1 }, 5*time.Second, 10*time.Millisecond)
	require.Never(t, func() bool { return len(driver.messagesIn("!journal")) > 1 }, 500*time.Millisecond, 20*time.Millisecond)
}

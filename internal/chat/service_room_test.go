package chat_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"sarak.sh/mikluko/roomcity/internal/chat"
	"sarak.sh/mikluko/roomcity/internal/chat/chattest"
	"sarak.sh/mikluko/roomcity/internal/session"
)

func notices(d *fakeDriver) []string {
	var out []string
	for _, m := range d.messages() {
		if m.Msg.Kind == chat.Notice {
			out = append(out, m.Msg.Body)
		}
	}
	return out
}

func TestServiceAnswersCommandsItself(t *testing.T) {
	sessions := &chattest.Sessions{}
	sessions.Add(alwaysOn("host", session.Running))
	driver, handle := runService(t, sessions)
	awaitRoom(t, sessions, handle)

	handle(chat.Incoming{Room: "!host", Sender: owner, Kind: chat.Text, Body: "!stop"})
	handle(chat.Incoming{Room: "!host", Sender: owner, Kind: chat.Text, Body: "!help"})
	handle(chat.Incoming{Room: "!host", Sender: owner, Kind: chat.Text, Body: "!what is this"})
	handle(chat.Incoming{Room: "!host", Sender: "@stranger:test.local", Kind: chat.Text, Body: "!stop"})

	require.Equal(t, 1, sessions.Cancels("s-host"), "a stranger's command does nothing")
	require.Empty(t, promptsOf(sessions), "no command reaches the agent")
	got := notices(driver)
	require.Len(t, got, 3)
	require.Contains(t, got[0], "stop")
	require.Contains(t, got[1], "`!stop`")
	require.Contains(t, got[2], "Unknown command `!what`")
	require.Contains(t, got[2], "`!help`", "an unknown command is answered with the known ones")
}

func permissionFrame(id string) session.Frame {
	return session.Frame{Direction: session.FromAgent, Message: []byte(
		`{"jsonrpc":"2.0","id":` + id + `,"method":"session/request_permission","params":{"toolCall":{"title":"Run it"},"options":[{"optionId":"allow","name":"Allow","kind":"allow_once"},{"optionId":"no","name":"Reject","kind":"reject_once"}]}}`)}
}

func TestServiceTakesAnOwnersReactionAsThePermissionAnswer(t *testing.T) {
	sessions := &chattest.Sessions{}
	sessions.Add(alwaysOn("host", session.Running))
	driver, handle := runService(t, sessions)
	awaitRoom(t, sessions, handle)
	sessions.Emit("s-host", permissionFrame("7"))

	var ask chat.MessageID
	require.Eventually(t, func() bool {
		for _, m := range driver.messages() {
			if m.Msg.Kind == chat.Text && strings.Contains(m.Msg.Body, "Permission needed") {
				ask = chat.MessageID("$" + m.Msg.Body)
			}
		}
		return ask != ""
	}, 5*time.Second, 10*time.Millisecond)

	handle(chat.Incoming{Room: "!host", Sender: "@stranger:test.local", Kind: chat.Reaction, To: ask, Key: "✅"})
	handle(chat.Incoming{Room: "!host", Sender: owner, Kind: chat.Reaction, To: ask, Key: "✅"})

	require.Eventually(t, func() bool { return len(sessions.Answers()) == 1 }, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, []chattest.PermissionAnswer{{Session: "s-host", RequestID: "7", OptionID: "allow"}}, sessions.Answers())
	require.Eventually(t, func() bool {
		return slices.Contains(notices(driver), "Answered by "+string(owner)+": Allow")
	}, 5*time.Second, 10*time.Millisecond, "the notice names who answered")
}

func TestServiceSaysSoWhenAPromptReachesNoRunningSession(t *testing.T) {
	for _, err := range []error{session.ErrNotRunning, session.ErrNotFound} {
		t.Run(err.Error(), func(t *testing.T) {
			sessions := &chattest.Sessions{}
			sessions.Add(alwaysOn("host", session.Ended))
			driver, handle := runService(t, sessions)
			require.Eventually(t, func() bool { return len(driver.roomSpecs()) == 1 }, 5*time.Second, 10*time.Millisecond)
			sessions.PromptErr = err
			require.Eventually(t, func() bool {
				handle(chat.Incoming{Room: "!host", Sender: owner, Kind: chat.Text, Body: "anyone there?", Message: "$owner"})
				return len(notices(driver)) > 0
			}, 5*time.Second, 10*time.Millisecond)
			require.Equal(t, "The message was not taken: the session is not running.", notices(driver)[0])
			require.Equal(t, chat.MessageID("$owner"), driver.messages()[0].Msg.ReplyTo)
		})
	}
}

func TestServiceMarksAnOwnersWaitingPromptAndClearsTheMarkWhenItIsTaken(t *testing.T) {
	sessions := &chattest.Sessions{}
	sessions.Add(alwaysOn("host", session.Running))
	driver, handle := runService(t, sessions)
	awaitRoom(t, sessions, handle)

	handle(chat.Incoming{Room: "!host", Sender: owner, Kind: chat.Text, Body: "steer", Message: "$steer"})
	seq := uint64(len(sessions.Prompts("s-host")))
	sessions.Emit("s-host",
		session.Prompt{Seq: seq, Source: session.FromOwner, Text: "steer", State: session.Stored},
		session.Prompt{Seq: 101, State: session.Waiting, Of: seq},
	)
	require.Eventually(t, func() bool {
		driver.mu.Lock()
		defer driver.mu.Unlock()
		return slices.Contains(driver.reactions, reaction{"$steer", "⏳"})
	}, 5*time.Second, 10*time.Millisecond, "the agent's user marks the owner's message")

	sessions.Emit("s-host", session.Prompt{Seq: 102, State: session.Delivered, Of: seq})
	require.Eventually(t, func() bool {
		driver.mu.Lock()
		defer driver.mu.Unlock()
		return len(driver.undone) == 1
	}, 5*time.Second, 10*time.Millisecond)
}

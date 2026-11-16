package chat_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"sarak.sh/mikluko/roomcity/internal/chat"
	"sarak.sh/mikluko/roomcity/internal/chat/chattest"
	"sarak.sh/mikluko/roomcity/internal/session"
)

func TestServiceStopEndsTheTypingOfAPromptThatNoTurnTook(t *testing.T) {
	sessions := &chattest.Sessions{}
	sessions.Add(alwaysOn("host", session.Running))
	driver, handle := runService(t, sessions)
	awaitRoom(t, sessions, handle)

	sessions.Emit("s-host", session.Prompt{Seq: 50, Source: session.FromOwner, Text: "hello", State: session.Stored})
	require.Eventually(t, func() bool {
		on, known := driver.lastTyping()
		return known && on
	}, 5*time.Second, 10*time.Millisecond, "the owner's message starts the typing notice")

	handle(chat.Incoming{Room: "!host", Sender: owner, Kind: chat.Text, Body: "!stop"})
	require.Eventually(t, func() bool {
		on, known := driver.lastTyping()
		return known && !on
	}, 5*time.Second, 10*time.Millisecond, "the stop ends it, though no turn was running to end")
}

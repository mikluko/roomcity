package chat_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"sarak.sh/mikluko/roomcity/internal/chat"
	"sarak.sh/mikluko/roomcity/internal/chat/chattest"
	"sarak.sh/mikluko/roomcity/internal/session"
)

func entriesOf(events ...session.Event) []session.Entry {
	at := time.Unix(1000, 0)
	out := make([]session.Entry, len(events))
	for i, ev := range events {
		out[i] = session.Entry{Time: at.Add(time.Duration(i) * time.Second), Event: ev}
	}
	return out
}

func agentFrame(json string) session.Frame {
	return session.Frame{Direction: session.FromAgent, Message: []byte(json)}
}

func TestTranscriptShowsTheOwnersWordsAndTheAgentsAnswerInOrder(t *testing.T) {
	items := chat.Transcript(entriesOf(
		session.Prompt{Seq: 1, Source: session.FromOwner, Text: "count the words", State: session.Stored},
		session.Prompt{Seq: 2, Source: session.FromGC, Text: "mail arrived", State: session.Stored},
		agentFrame(`{"method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Nine."}}}}`),
		agentFrame(`{"id":3,"result":{"stopReason":"end_turn"}}`),
	))
	require.Len(t, items, 3)
	require.Equal(t, "owner", items[0].From)
	require.Equal(t, "count the words", items[0].Message.Body)
	require.Equal(t, "From gc: mail arrived", items[1].Message.Body)
	require.Equal(t, "Nine.", items[2].Message.Body)
	require.Equal(t, time.Unix(1003, 0), items[2].Time, "an item has the time of the event that completed it: the turn's end")
}

func TestTranscriptKeepsWhatARoomCutsShort(t *testing.T) {
	long := strings.Repeat("x", 9000)
	items := chat.Transcript(entriesOf(
		agentFrame(`{"method":"session/update","params":{"update":{"sessionUpdate":"tool_call","toolCallId":"t","title":"Run","status":"pending","rawInput":{"command":"c"}}}}`),
		agentFrame(`{"method":"session/update","params":{"update":{"sessionUpdate":"tool_call_update","toolCallId":"t","status":"completed","content":[{"type":"content","content":{"type":"text","text":"`+long+`"}}]}}}`),
	))
	require.Len(t, items, 2)
	require.Equal(t, long, items[1].Message.Fold.Text, "the whole output, not its first 4000 bytes")
}

func TestTranscriptShowsThePlanAsItStoodAtTheEndAndAnAnsweredPermission(t *testing.T) {
	items := chat.Transcript(entriesOf(
		agentFrame(`{"method":"session/update","params":{"update":{"sessionUpdate":"plan","entries":[{"content":"a","status":"pending"}]}}}`),
		agentFrame(`{"method":"session/update","params":{"update":{"sessionUpdate":"plan","entries":[{"content":"a","status":"completed"}]}}}`),
		agentFrame(`{"id":7,"method":"session/request_permission","params":{"toolCall":{"title":"Run it"},"options":[{"optionId":"allow","name":"Allow","kind":"allow_once"}]}}`),
		session.Frame{Direction: session.ToAgent, Message: []byte(`{"id":7,"result":{"outcome":{"outcome":"selected","optionId":"allow"}}}`)},
	))
	var bodies []string
	for _, it := range items {
		bodies = append(bodies, it.Message.Body)
	}
	require.Equal(t, "**Plan** 1/1\n\n- ☑ ~~a~~", bodies[0], "one plan, as it ended")
	require.Contains(t, bodies[1], "Permission needed")
	require.Equal(t, "Answered: Allow", bodies[2])
	require.Len(t, bodies, 3)
}

func TestTranscriptOfARecordedSessionHoldsItsWholeAnswer(t *testing.T) {
	events, err := chattest.Recording("testdata/counter.acp.jsonl")
	require.NoError(t, err)
	items := chat.Transcript(entriesOf(events...))
	want := answerText(t, events)
	var got strings.Builder
	for _, it := range items {
		if it.Message.Kind == chat.Text && !strings.HasPrefix(it.Message.Body, "🔐") {
			got.WriteString(it.Message.Body)
		}
	}
	require.Equal(t, want, got.String())
	require.Greater(t, len(items), 10, fmt.Sprintf("%d items", len(items)))
}

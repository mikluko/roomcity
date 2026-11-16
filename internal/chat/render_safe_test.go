package chat

import (
	"testing"

	"github.com/stretchr/testify/require"

	"sarak.sh/mikluko/roomcity/internal/session"
)

func numbered(f session.Frame, seq uint64) session.Frame {
	f.Seq = seq
	return f
}

func textAt(text string, seq uint64) session.Frame {
	return numbered(update("agent_message_chunk", `"content":{"type":"text","text":"`+text+`"}`), seq)
}

func TestSafeIsTheEventBeforeTheOldestTextHeldBack(t *testing.T) {
	r, out, _ := newTestRenderer()

	r.event(textAt("first\\n\\nsecond ", 5))
	require.Equal(t, []string{"first"}, bodies(out.sent))
	require.EqualValues(t, 4, r.safe(5), "the second paragraph began in event 5")

	r.event(textAt("goes on", 6))
	require.EqualValues(t, 4, r.safe(6), "and is still being written")

	r.event(textAt("\\n\\nthird", 7))
	require.Equal(t, []string{"first", "second goes on"}, bodies(out.sent))
	require.EqualValues(t, 6, r.safe(7), "the third began in event 7; nothing before it is held")

	r.event(numbered(endOfTurn(), 8))
	require.Equal(t, []string{"first", "second goes on", "third"}, bodies(out.sent))
	require.EqualValues(t, 8, r.safe(8), "the turn's end leaves nothing held")
}

func TestSafeHoldsThoughtsAndAPlanEditUntilTheyAreSent(t *testing.T) {
	r, _, _ := newTestRenderer()

	r.event(numbered(update("agent_thought_chunk", `"content":{"type":"text","text":"hmm"}`), 3))
	require.EqualValues(t, 2, r.safe(3))
	r.event(textAt("answer", 4))
	require.EqualValues(t, 3, r.safe(4), "the thoughts are sent when the answer begins; the answer's text is held")
}

func TestAToolNoticeAndAPermissionRequestAreNotSentBeforeThePlanEditThatWaits(t *testing.T) {
	r, out, _ := newTestRenderer()
	r.event(numbered(plan("pending", "pending"), 1))
	r.event(numbered(plan("completed", "pending"), 2))
	require.Len(t, out.sent, 1, "the edit waits for its gap")
	require.EqualValues(t, 1, r.safe(2))

	r.event(numbered(update("tool_call", `"toolCallId":"t1","title":"Run","status":"pending","rawInput":{"a":1}`), 3))
	require.Len(t, out.sent, 3, "the plan's edit, then the tool's notice")
	require.Equal(t, MessageID("m1"), out.sent[1].Replaces)
	require.Equal(t, "🔧 **Run**", out.sent[2].Body)
	require.EqualValues(t, 3, r.safe(3), "nothing is held: a restart now shows neither again")

	r.event(numbered(plan("completed", "completed"), 4))
	r.event(numbered(permissionRequest("9", "Run it"), 5))
	require.Len(t, out.sent, 5)
	require.Equal(t, MessageID("m1"), out.sent[3].Replaces, "the edit first")
	require.Contains(t, out.sent[4].Body, "Permission needed")
	require.EqualValues(t, 5, r.safe(5))
}

package chat

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"sarak.sh/mikluko/roomcity/internal/session"
)

func TestSplitParagraphs(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		wantDone []string
		wantRest string
	}{
		{"nothing closed", "one line", nil, "one line"},
		{"one closed", "first\n\nsecond", []string{"first"}, "second"},
		{"several lines in a paragraph", "a\nb\n\nc\n\n", []string{"a\nb", "c"}, ""},
		{"a fence keeps its blank lines", "```go\nx\n\ny\n```\n\nafter", []string{"```go\nx\n\ny\n```"}, "after"},
		{"an open fence is not closed", "```\nx\n\ny", nil, "```\nx\n\ny"},
		{"extra blank lines", "a\n\n\n\nb\n\n", []string{"a", "b"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			done, rest := splitParagraphs(tt.text)
			require.Equal(t, tt.wantDone, done)
			require.Equal(t, tt.wantRest, rest)
		})
	}
}

func TestCutAtLines(t *testing.T) {
	pair := "abcdefghi\nabcdefghi"
	tests := []struct {
		name string
		text string
		max  int
		want []string
	}{
		{"short", "a\nb", 10, []string{"a\nb"}},
		{"cut at line ends", "aaa\nbbb\nccc", 8, []string{"aaa\nbbb", "ccc"}},
		{"every piece fits", strings.Repeat("abcdefghi\n", 10), 25, []string{pair, pair, pair, pair, pair}},
		{"one line over the limit is cut inside", "abcdefgh", 3, []string{"abc", "def", "gh"}},
		{"a rune is not cut", "ééé", 3, []string{"é", "é", "é"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, cutAtLines(tt.text, tt.max))
		})
	}
}

func chunk(text string) session.Frame {
	return session.Frame{Direction: session.FromAgent, Message: []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":` + quote(text) + `}}}}`)}
}

func quote(s string) string {
	s = strings.ReplaceAll(s, "\n", `\n`)
	return `"` + s + `"`
}

func TestAnswerSendsAParagraphWhenItCloses(t *testing.T) {
	var a answer
	require.Empty(t, a.feed(chunk("First para")))
	require.Empty(t, a.feed(chunk("graph.\n")))
	require.Equal(t, []string{"First paragraph."}, a.feed(chunk("\nSecond")))
}

func TestAnswerFlushesWhatIsUnsent(t *testing.T) {
	tests := []struct {
		name  string
		ev    session.Event
		flush bool
	}{
		{"the turn ends", session.Frame{Direction: session.FromAgent, Message: []byte(`{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn"}}`)}, true},
		{"a tool call starts", session.Frame{Direction: session.FromAgent, Message: []byte(`{"method":"session/update","params":{"update":{"sessionUpdate":"tool_call"}}}`)}, true},
		{"a tool call starts with its content as a list", session.Frame{Direction: session.FromAgent, Message: []byte(`{"method":"session/update","params":{"update":{"sessionUpdate":"tool_call","content":[{"type":"content"}]}}}`)}, true},
		{"a permission is asked", session.Frame{Direction: session.FromAgent, Message: []byte(`{"method":"session/request_permission","id":9}`)}, true},
		{"the session ends", session.Lifecycle{Kind: session.Stopped}, true},
		{"a thought arrives", session.Frame{Direction: session.FromAgent, Message: []byte(`{"method":"session/update","params":{"update":{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"hm"}}}}`)}, false},
		{"the owner's prompt goes to the agent", session.Frame{Direction: session.ToAgent, Message: []byte(`{"method":"session/prompt","id":3}`)}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var a answer
			a.feed(chunk("unfinished"))
			got := a.feed(tt.ev)
			if tt.flush {
				require.Equal(t, []string{"unfinished"}, got)
			} else {
				require.Empty(t, got)
			}
		})
	}
}

func TestAnswerIgnoresWhatItCannotRead(t *testing.T) {
	var a answer
	require.Empty(t, a.feed(session.Frame{Direction: session.FromAgent, Message: []byte(`not json`)}))
	require.Empty(t, a.feed(session.Frame{Direction: session.FromAgent, Message: []byte(`{"method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"image"}}}}`)}))
}

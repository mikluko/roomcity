package screen_test

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"sarak.sh/mikluko/roomcity/internal/screen"
	"sarak.sh/mikluko/roomcity/internal/session"
)

func update(kind, extra string) session.Event {
	return session.Frame{Direction: session.FromAgent, Message: []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"` + kind + `"` + extra + `}}}`)}
}

func chunk(text string) session.Event {
	return update("agent_message_chunk", `,"content":{"type":"text","text":`+quote(text)+`}`)
}

func quote(s string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), "\n", `\n`) + `"`
}

func TestAnIdleScreenEndsWithTheReadyPrompt(t *testing.T) {
	got := screen.Render("claude --model sonnet", []session.Event{
		session.Prompt{Source: session.FromOwner, Text: "count the words", State: session.Stored},
		session.Prompt{Source: session.FromOwner, Text: "count the words", State: session.Delivered},
		chunk("I'll "), chunk("count them.\nThere are 4."),
		update("tool_call", `,"title":"Read counter.go"`),
		chunk("Done."),
	}, 0, false, "")
	require.Equal(t, []string{"$ claude --model sonnet", "> count the words", "I'll count them.", "There are 4.", "• Read counter.go", "Done.", "❯ "}, got)
}

func TestARunningTurnEndsWithTheBusyLineAndNotThePrompt(t *testing.T) {
	got := screen.Render("", []session.Event{chunk("working")}, 0, true, "❯ ")
	require.Equal(t, []string{"working", screen.Busy}, got)
	require.NotContains(t, strings.Join(got, "\n"), "❯ ")
}

func TestAGcNudgeIsShownAsGcsAndThePromptIsGcsOwn(t *testing.T) {
	got := screen.Render("", []session.Event{
		session.Prompt{Source: session.FromGC, Text: "mail waits", State: session.Delivered},
	}, 0, false, "$ ")
	require.Equal(t, []string{"[gc] mail waits", "$ "}, got)
}

func TestOnlyTheLastLinesAreKept(t *testing.T) {
	var events []session.Event
	for _, l := range []string{"a", "b", "c", "d"} {
		events = append(events, chunk(l+"\n"), update("tool_call", `,"title":"`+l+`"`))
	}
	got := screen.Render("", events, 4, false, "")
	require.Len(t, got, 4)
	require.Equal(t, "❯ ", got[3])
	require.Equal(t, "• d", got[2])
	require.Equal(t, []string{"❯ "}, screen.Render("", events, 1, false, ""))
}

func TestWhatIsNotShownIsLeftOut(t *testing.T) {
	got := screen.Render("", []session.Event{
		update("agent_thought_chunk", `,"content":{"type":"text","text":"thinking"}`),
		update("usage_update", ""),
		session.Frame{Direction: session.ToAgent, Message: []byte(`{"method":"session/prompt","id":3}`)},
		session.Frame{Direction: session.FromAgent, Message: []byte(`not json`)},
		session.Prompt{Source: session.FromOwner, Text: "waits", State: session.Waiting},
	}, 0, false, "")
	require.Equal(t, []string{"❯ "}, got)
}

func TestARequestForPermissionAndALifecycleChangeAreShown(t *testing.T) {
	got := screen.Render("", []session.Event{
		session.Lifecycle{Kind: session.StartedAfresh},
		session.Frame{Direction: session.FromAgent, Message: []byte(`{"id":5,"method":"session/request_permission","params":{"toolCall":{"title":"Run rm -rf"}}}`)},
	}, 0, true, "")
	require.Equal(t, []string{"-- session started afresh --", "? permission: Run rm -rf", screen.Busy}, got)
}

func TestEveryToolCallOfARealStreamIsShown(t *testing.T) {
	f, err := os.Open("../operate/testdata/counter.acp.jsonl")
	require.NoError(t, err)
	defer f.Close()
	var events []session.Event
	calls := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var line struct {
			Dir string          `json:"dir"`
			Msg json.RawMessage `json:"msg"`
		}
		require.NoError(t, json.Unmarshal(sc.Bytes(), &line))
		d := session.FromAgent
		if line.Dir == "out" {
			d = session.ToAgent
		}
		if d == session.FromAgent && strings.Contains(string(line.Msg), `"sessionUpdate":"tool_call"`) {
			calls++
		}
		events = append(events, session.Frame{Direction: d, Message: line.Msg})
	}
	require.NoError(t, sc.Err())
	require.Equal(t, 6, calls, "the recording's tool calls")

	shown := 0
	for _, l := range screen.Render("", events, 0, false, "") {
		if strings.HasPrefix(l, "• ") {
			shown++
		}
	}
	require.Equal(t, calls, shown)
}

package chat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"sarak.sh/mikluko/roomcity/internal/session"
)

func TestRendererSaysWhatTheRoomsWorkToolsDid(t *testing.T) {
	result := "Fixed the parser.\nTests pass."
	tests := []struct {
		tool   string
		input  map[string]any
		output string
		body   string
		fold   *Fold
	}{
		{
			tool:   "dispatch",
			input:  map[string]any{"task": "Fix the parser\nKeep its API"},
			output: "Handed to a worker as task sa-8uw.",
			body:   "Handed to a worker: Fix the parser (task sa-8uw)",
		},
		{
			tool:   "wait",
			input:  map[string]any{"task": "sa-8uw"},
			output: "You will be told when task sa-8uw is closed.",
			body:   "Waiting to be told when task sa-8uw is closed",
		},
		{
			tool:   "result",
			input:  map[string]any{"task": "sa-8uw"},
			output: result,
			body:   "Read the result of task sa-8uw",
			fold:   &Fold{Summary: "output", Text: result, Code: true},
		},
		{
			tool:   "claim",
			output: "Task sa-8uw:\nFix the parser\nKeep its API",
			body:   "Took task sa-8uw: Fix the parser",
		},
		{
			tool:   "done",
			input:  map[string]any{"result": result},
			output: "The task is closed; this session is ending.",
			body:   "Closed its task with the result",
			fold:   &Fold{Summary: "output", Text: result, Code: true},
		},
	}

	for _, agent := range []string{"claude", "goose"} {
		for _, tt := range tests {
			t.Run(agent+"/"+tt.tool, func(t *testing.T) {
				r, out, _ := newTestRenderer()
				frames := recordedToolCall(t, agent, "room", tt.tool, "completed", tt.input, tt.output)
				for _, f := range frames[:len(frames)-1] {
					r.event(f)
				}
				require.Empty(t, out.sent, "a work tool has no bare start notice")

				r.event(frames[len(frames)-1])

				require.Equal(t, []Message{{Kind: Notice, Body: tt.body, Fold: tt.fold}}, out.sent)
			})
		}
	}
}

func TestRendererLeavesOtherToolsWithTheSameNameAlone(t *testing.T) {
	for _, tt := range []struct {
		agent, server, tool string
	}{
		{agent: "claude", server: "other", tool: "dispatch"},
		{agent: "goose", server: "other", tool: "dispatch"},
		{agent: "claude", server: "room", tool: "attach"},
		{agent: "goose", server: "room", tool: "attach"},
	} {
		t.Run(tt.agent+"/"+tt.server+"/"+tt.tool, func(t *testing.T) {
			r, out, _ := newTestRenderer()
			for _, f := range recordedToolCall(t, tt.agent, tt.server, tt.tool, "completed", map[string]any{"task": "x"}, "answer") {
				r.event(f)
			}
			_, title := recordedToolName(t, tt.agent, tt.server, tt.tool)
			require.Equal(t, []string{"🔧 **" + title + "**", "✅ done"}, bodies(out.sent))
		})
	}

	r, out, _ := newTestRenderer()
	r.event(update("tool_call", `"toolCallId":"t","title":"room: dispatch","kind":"other","status":"pending","rawInput":{"task":"x"}`))
	r.event(update("tool_call_update", `"toolCallId":"t","status":"completed","content":[{"type":"content","content":{"type":"text","text":"answer"}}]`))
	require.Equal(t, []string{"🔧 **room: dispatch**", "✅ done"}, bodies(out.sent), "a display title alone is not a room tool's name")
}

func TestRendererKeepsAWorkToolsFailureVisible(t *testing.T) {
	tests := []struct {
		tool  string
		input map[string]any
		body  string
	}{
		{tool: "dispatch", input: map[string]any{"task": "Fix the parser\nKeep its API"}, body: "❌ Could not hand to a worker: Fix the parser"},
		{tool: "wait", input: map[string]any{"task": "sa-8uw"}, body: "❌ Could not wait for task sa-8uw"},
		{tool: "result", input: map[string]any{"task": "sa-8uw"}, body: "❌ Could not read the result of task sa-8uw"},
		{tool: "claim", body: "❌ Could not take a task"},
		{tool: "done", input: map[string]any{"result": "not closed"}, body: "❌ Could not close its task"},
	}
	for _, agent := range []string{"claude", "goose"} {
		for _, tt := range tests {
			t.Run(agent+"/"+tt.tool, func(t *testing.T) {
				r, out, _ := newTestRenderer()
				frames := recordedToolCall(t, agent, "room", tt.tool, "failed", tt.input, "database is down")
				for _, f := range frames {
					r.event(f)
				}
				require.Equal(t, []Message{{
					Kind: Notice,
					Body: tt.body,
					Fold: &Fold{Summary: "output", Text: "database is down", Code: true},
				}}, out.sent)
			})
		}
	}
}

func TestRendererKeepsADeniedWorkToolsActVisible(t *testing.T) {
	for _, denial := range []string{"cancelled", "refused"} {
		t.Run(denial, func(t *testing.T) {
			r, out, _ := newTestRenderer()
			frames := recordedToolCall(t, "claude", "room", "done", "failed", map[string]any{"result": "not closed"}, "permission denied")
			for _, f := range frames[:len(frames)-1] {
				r.event(f)
			}
			r.denied["tool-1"] = denial
			r.event(frames[len(frames)-1])
			require.Equal(t, []Message{{Kind: Notice, Body: "❌ Could not close its task"}}, out.sent)
		})
	}
}

func TestRendererSaysWhenAClaimAnswerCannotBeRead(t *testing.T) {
	r, out, _ := newTestRenderer()
	for _, f := range recordedToolCall(t, "claude", "room", "claim", "completed", nil, "unexpected answer") {
		r.event(f)
	}
	require.Equal(t, []Message{{
		Kind: Notice,
		Body: "❌ Could not take a task",
		Fold: &Fold{Summary: "output", Text: "unexpected answer", Code: true},
	}}, out.sent)
}

func TestRendererCompletesAClaudeWorkNoticeFromItsEndingFrame(t *testing.T) {
	tests := []struct {
		tool   string
		input  map[string]any
		output string
		body   string
	}{
		{tool: "dispatch", output: "Handed to a worker as task sa-8uw.", body: "Handed to a worker (task sa-8uw)"},
		{tool: "wait", output: "You will be told when task sa-8uw is closed.", body: "Waiting to be told when the task is closed"},
		{tool: "wait", input: map[string]any{"task": "sa-8uw"}, output: "You will be told when task sa-8uw is closed.", body: "Waiting to be told when task sa-8uw is closed"},
		{tool: "result", output: "Fixed the parser.", body: "Read the result of the task"},
		{tool: "result", input: map[string]any{"task": "sa-8uw"}, output: "Fixed the parser.", body: "Read the result of task sa-8uw"},
	}
	for _, tt := range tests {
		t.Run(tt.body, func(t *testing.T) {
			r, out, _ := newTestRenderer()
			name, title := recordedToolName(t, "claude", "room", tt.tool)
			u := map[string]any{
				"sessionUpdate": "tool_call_update",
				"toolCallId":    "tool-1",
				"title":         title,
				"status":        "completed",
				"rawOutput":     tt.output,
				"content": []any{map[string]any{
					"type":    "content",
					"content": map[string]any{"type": "text", "text": tt.output},
				}},
				"_meta": recordedToolMeta(t, "claude", name),
			}
			if tt.input != nil {
				u["rawInput"] = tt.input
			}
			r.event(recordedToolFrame(t, u))
			require.Equal(t, tt.body, out.sent[0].Body)
		})
	}
}

func TestRendererSaysWhenAWorkerHadNoTaskToTake(t *testing.T) {
	r, out, _ := newTestRenderer()
	for _, f := range recordedToolCall(t, "goose", "room", "claim", "completed", nil, "There is no task for you; this session is ending.") {
		r.event(f)
	}
	require.Equal(t, []string{"No task to take; its session is ending"}, bodies(out.sent))
}

func TestRendererCutsATaskToOneShortLine(t *testing.T) {
	long := strings.Repeat("x", maxRoomTask+20) + "\nnot shown"
	r, out, _ := newTestRenderer()
	for _, f := range recordedToolCall(t, "claude", "room", "dispatch", "completed", map[string]any{"task": long}, "Handed to a worker as task sa-8uw.") {
		r.event(f)
	}
	require.Equal(t, []string{"Handed to a worker: " + strings.Repeat("x", maxRoomTask) + "… (task sa-8uw)"}, bodies(out.sent))
	require.Equal(t, "é…", taskLine("éé", 3), "a cut does not split a rune")
}

func recordedToolCall(t *testing.T, agent, server, tool, status string, input map[string]any, output string) []session.Frame {
	t.Helper()
	name, title := recordedToolName(t, agent, server, tool)
	meta := recordedToolMeta(t, agent, name)
	if input == nil {
		input = map[string]any{}
	}
	start := map[string]any{
		"sessionUpdate": "tool_call",
		"toolCallId":    "tool-1",
		"title":         title,
		"kind":          "other",
		"status":        "pending",
		"rawInput":      map[string]any{},
		"content":       []any{},
		"_meta":         meta,
	}
	updates := []map[string]any{start}
	if agent == "goose" {
		start["rawInput"] = input
	} else {
		updates = append(updates, map[string]any{
			"sessionUpdate": "tool_call_update",
			"toolCallId":    "tool-1",
			"title":         title,
			"kind":          "other",
			"rawInput":      input,
			"_meta":         meta,
		})
	}
	end := map[string]any{
		"sessionUpdate": "tool_call_update",
		"toolCallId":    "tool-1",
		"status":        status,
		"rawOutput":     output,
		"content": []any{map[string]any{
			"type": "content",
			"content": map[string]any{
				"type": "text",
				"text": output,
			},
		}},
	}
	if agent == "claude" {
		end["_meta"] = meta
	}
	updates = append(updates, end)
	frames := make([]session.Frame, len(updates))
	for i, u := range updates {
		frames[i] = recordedToolFrame(t, u)
	}
	return frames
}

func recordedToolName(t *testing.T, agent, server, tool string) (name, title string) {
	t.Helper()
	switch agent {
	case "claude":
		name = "mcp__" + server + "__" + tool
		return name, name
	case "goose":
		return server + "__" + tool, server + ": " + tool
	default:
		t.Fatalf("unknown agent %q", agent)
		return "", ""
	}
}

func recordedToolMeta(t *testing.T, agent, name string) map[string]any {
	t.Helper()
	switch agent {
	case "claude":
		return map[string]any{"claudeCode": map[string]any{"toolName": name}}
	case "goose":
		return map[string]any{"goose": map[string]any{"toolCall": map[string]any{"toolName": name}}}
	default:
		t.Fatalf("unknown agent %q", agent)
		return nil
	}
}

func recordedToolFrame(t *testing.T, u map[string]any) session.Frame {
	t.Helper()
	msg, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "session/update",
		"params": map[string]any{
			"sessionId": "s",
			"update":    u,
		},
	})
	require.NoError(t, err)
	return fromAgent(string(msg))
}

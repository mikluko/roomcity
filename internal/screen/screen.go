// Package screen renders a session's stream as the text of a screen, which is
// what gc's operations "peek" and the idle boundary read: the last lines of
// what the room shows, then a line that says whether a turn is running.
//
// The screen is no terminal and shows none of one's furniture. It has two
// contracts with gc. The last line is the ready prompt when no turn is
// running, which gc's idle scan looks for; while a turn runs, which includes a
// turn that waits for a permission answer, the last line is the busy
// indicator gc's scan reads as work in progress.
package screen

import (
	"encoding/json"
	"strings"

	"sarak.sh/mikluko/roomcity/internal/session"
)

// DefaultPrompt is the ready prompt gc looks for when its configuration names
// none.
const DefaultPrompt = "❯ "

// Busy is the last line while a turn runs.
const Busy = "✻ Working… (esc to interrupt)"

// MaxLines bounds what Render keeps of the stream's text.
const MaxLines = 500

// Render returns the last lines (all of them when lines <= 0, at most
// MaxLines) of the text the events show, ending with the ready prompt, or
// with the busy line when turn says a turn runs. A screen begins with the
// command line the agent was started with, as a terminal's does, when
// command is not "": it scrolls away as the text grows.
func Render(command string, events []session.Event, lines int, turn bool, prompt string) []string {
	if prompt == "" {
		prompt = DefaultPrompt
	}
	var text []string
	if command != "" {
		text = append(text, "$ "+command)
	}
	var agent strings.Builder
	flush := func() {
		if agent.Len() > 0 {
			text = append(text, strings.Split(strings.TrimRight(agent.String(), "\n"), "\n")...)
			agent.Reset()
		}
	}
	for _, e := range events {
		switch e := e.(type) {
		case session.Prompt:
			if e.State != session.Delivered {
				continue
			}
			flush()
			label := "> "
			if e.Source == session.FromGC {
				label = "[gc] "
			}
			text = append(text, label+strings.ReplaceAll(strings.TrimRight(e.Text, "\n"), "\n", "\n  "))
		case session.Frame:
			if e.Direction != session.FromAgent {
				continue
			}
			if chunk, line := fromAgent(e.Message); chunk != "" {
				agent.WriteString(chunk)
			} else if line != "" {
				flush()
				text = append(text, line)
			}
		case session.Lifecycle:
			flush()
			text = append(text, lifecycleLine(e.Kind))
		}
	}
	flush()

	bottom := prompt
	if turn {
		bottom = Busy
	}
	keep := MaxLines
	if lines > 0 && lines-1 < keep {
		keep = max(lines-1, 0)
	}
	if len(text) > keep {
		text = text[len(text)-keep:]
	}
	if lines == 1 || (lines > 0 && keep == 0) {
		return []string{bottom}
	}
	return append(text, bottom)
}

func lifecycleLine(k session.LifecycleKind) string {
	switch k {
	case session.Started:
		return "-- session started --"
	case session.StartedAfresh:
		return "-- session started afresh --"
	}
	return "-- session ended --"
}

// fromAgent reads one message of the agent: a chunk of its answer, or a line
// to show on its own (a tool call, a request for permission). A message that
// shows nothing is "", "".
func fromAgent(message []byte) (chunk, line string) {
	var m struct {
		Method string `json:"method"`
		Params struct {
			Update struct {
				Kind  string `json:"sessionUpdate"`
				Title string `json:"title"`
				// Content is an object for a chunk of the agent's words and a
				// list for a tool call: it is read only where it is the former.
				Content json.RawMessage `json:"content"`
			} `json:"update"`
			ToolCall struct {
				Title string `json:"title"`
			} `json:"toolCall"`
		} `json:"params"`
	}
	if json.Unmarshal(message, &m) != nil {
		return "", ""
	}
	switch m.Method {
	case "session/update":
		switch m.Params.Update.Kind {
		case "agent_message_chunk":
			var c struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(m.Params.Update.Content, &c) != nil {
				return "", ""
			}
			return c.Text, ""
		case "tool_call":
			return "", "• " + m.Params.Update.Title
		}
	case "session/request_permission":
		return "", "? permission: " + m.Params.ToolCall.Title
	}
	return "", ""
}

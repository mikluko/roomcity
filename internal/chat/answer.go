package chat

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"sarak.sh/mikluko/roomcity/internal/session"
)

// maxMessageBytes bounds one message's text, well under the 65535 bytes
// Tuwunel allows an event once the HTML copy of the text is added.
const maxMessageBytes = 16000

// answer turns the ACP frames of one agent's session into the paragraphs of
// its answer. It is not safe for concurrent use.
type answer struct {
	pending string
}

// feed takes the next event of the session and returns the messages that are
// now complete, in order.
func (a *answer) feed(ev session.Event) []string {
	switch ev := ev.(type) {
	case session.Frame:
		return a.frame(ev)
	case session.Lifecycle:
		if ev.Kind == session.Stopped || ev.Kind == session.StartedAfresh {
			return a.flush()
		}
	}
	return nil
}

func (a *answer) frame(f session.Frame) []string {
	if f.Direction != session.FromAgent {
		return nil
	}
	var m struct {
		Method string `json:"method"`
		Params struct {
			Update struct {
				Kind string `json:"sessionUpdate"`
				// Content is an object for a message chunk and a list for a
				// tool call's output, so it is read only where it is one.
				Content json.RawMessage `json:"content"`
			} `json:"update"`
		} `json:"params"`
		Result *struct {
			StopReason string `json:"stopReason"`
		} `json:"result"`
	}
	if err := json.Unmarshal(f.Message, &m); err != nil {
		return nil
	}
	switch {
	case m.Method == "session/update" && m.Params.Update.Kind == "agent_message_chunk":
		var content struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(m.Params.Update.Content, &content) != nil || content.Type != "text" {
			return nil
		}
		a.pending += content.Text
		return a.complete()
	case m.Method == "session/update" && m.Params.Update.Kind == "tool_call",
		m.Method == "session/request_permission":
		return a.flush()
	case m.Method == "" && m.Result != nil && m.Result.StopReason != "":
		return a.flush()
	}
	return nil
}

// complete returns the paragraphs that a blank line has closed.
func (a *answer) complete() []string {
	done, rest := splitParagraphs(a.pending)
	a.pending = rest
	return cutLong(done)
}

// flush returns everything unsent, closed or not.
func (a *answer) flush() []string {
	done, _ := splitParagraphs(a.pending + "\n\n")
	a.pending = ""
	return cutLong(done)
}

// splitParagraphs cuts text at blank lines outside code fences. rest holds
// whatever a blank line has not closed yet.
func splitParagraphs(text string) (done []string, rest string) {
	var cur []string
	fenced := false
	for {
		i := strings.IndexByte(text, '\n')
		if i < 0 {
			break
		}
		line := text[:i]
		text = text[i+1:]
		switch {
		case strings.HasPrefix(strings.TrimSpace(line), "```"):
			fenced = !fenced
			cur = append(cur, line)
		case !fenced && strings.TrimSpace(line) == "":
			if len(cur) > 0 {
				done = append(done, strings.Join(cur, "\n"))
				cur = nil
			}
		default:
			cur = append(cur, line)
		}
	}
	var b strings.Builder
	for _, l := range cur {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	return done, b.String() + text
}

// cutLong splits each paragraph over maxMessageBytes at line ends.
func cutLong(paragraphs []string) []string {
	var out []string
	for _, p := range paragraphs {
		out = append(out, cutAtLines(p, maxMessageBytes)...)
	}
	return out
}

// cutAtLines splits text into pieces of at most max bytes, each ending at a
// line end. A single line over max is cut at max.
func cutAtLines(text string, max int) []string {
	if len(text) <= max {
		return []string{text}
	}
	var out []string
	var cur bytes.Buffer
	for _, line := range strings.SplitAfter(text, "\n") {
		for len(line) > max {
			if cur.Len() > 0 {
				out = append(out, strings.TrimSuffix(cur.String(), "\n"))
				cur.Reset()
			}
			n := max
			for n > 0 && !utf8.RuneStart(line[n]) {
				n--
			}
			out = append(out, line[:n])
			line = line[n:]
		}
		if cur.Len() > 0 && cur.Len()+len(line) > max {
			out = append(out, strings.TrimSuffix(cur.String(), "\n"))
			cur.Reset()
		}
		cur.WriteString(line)
	}
	if cur.Len() > 0 {
		out = append(out, strings.TrimSuffix(cur.String(), "\n"))
	}
	return out
}

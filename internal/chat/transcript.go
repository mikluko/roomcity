package chat

import (
	"fmt"
	"time"

	"sarak.sh/mikluko/roomcity/internal/session"
)

// Item is one message of a transcript: what the room showed, or would have,
// with the time of the event that made it and who it is from.
type Item struct {
	Time time.Time
	// From is "owner" for what the owner wrote, and "agent" for what the
	// agent's user showed in the room: its answer, its thoughts, its tool
	// calls and its plan, and the notices of the runtime.
	From    string
	Message Message
}

// Transcript renders a session's recorded stream as the room shows a
// session, but whole: a tool call's output and a permission request's detail
// are not cut, and the owner's own words are in, which the room has as the
// owner's own messages. A plan is shown as it stood at the end, since the room
// edits it in place.
func Transcript(entries []session.Entry) []Item {
	out := &transcriptOut{}
	r := newRenderer(out, func() time.Time { return out.at }, 0)
	r.toolLimit, r.detailLimit = 0, 0
	for _, e := range entries {
		out.at = e.Time
		if p, ok := e.Event.(session.Prompt); ok && p.State == session.Stored && p.Source == session.FromOwner {
			out.add("owner", Message{Kind: Text, Body: p.Text})
		}
		if f, ok := e.Event.(session.File); ok {
			out.add("agent", Message{Kind: Notice, Body: fmt.Sprintf("Sent a file: %s (%s, %d bytes)", f.Name, f.MediaType, f.Size)})
		}
		r.event(e.Event)
	}
	r.drain()
	return out.items
}

// transcriptOut is the outbox of a renderer that writes no room: it keeps
// the messages in order, and applies an edit to the message it replaces.
type transcriptOut struct {
	at    time.Time
	items []Item
}

func (o *transcriptOut) add(from string, m Message) MessageID {
	o.items = append(o.items, Item{Time: o.at, From: from, Message: m})
	return MessageID(fmt.Sprintf("m%d", len(o.items)))
}

func (o *transcriptOut) send(m Message) MessageID {
	if m.Replaces != "" {
		for i := range o.items {
			if id := MessageID(fmt.Sprintf("m%d", i+1)); id == m.Replaces {
				m.Replaces = ""
				o.items[i].Message = m
				return id
			}
		}
	}
	return o.add("agent", m)
}

func (*transcriptOut) react(MessageID, string) ReactionID          { return "" }
func (*transcriptOut) unreact(ReactionID)                          {}
func (*transcriptOut) typing(bool)                                 {}
func (*transcriptOut) promptMessage(uint64) MessageID              { return "" }
func (*transcriptOut) forgetPrompt(uint64)                         {}
func (*transcriptOut) answerPermission(requestID, optionID string) {}

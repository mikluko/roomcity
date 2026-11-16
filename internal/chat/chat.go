// Package chat connects the sessions of a city to a chat system: a room for
// each always-on agent, the owner's messages in as prompts, the agent's
// answer out as messages.
//
// Nothing outside a Driver knows which chat system is behind it.
package chat

import "context"

// UserID names a person or a program in the chat system, in its own form.
type UserID string

// RoomID names a room in the chat system.
type RoomID string

// MessageID names a message sent to a room.
type MessageID string

// Kind says how a message is meant to be read.
type Kind int

const (
	// Text is a message that notifies: an answer, a question to the owner.
	Text Kind = iota
	// Notice is a message that does not notify: a log line, a status.
	Notice
)

// Message is what a speaker sends to a room.
type Message struct {
	Kind Kind
	// Body is Markdown.
	Body string
	// ReplyTo, when set, makes the message a reply to that message.
	ReplyTo MessageID
	// Replaces, when set, makes the message an edit of that message, which
	// must have been sent by the same speaker.
	Replaces MessageID
}

// RoomSpec describes the room of one agent.
type RoomSpec struct {
	// Key names the room within the city and is what finds it again: the
	// agent's name.
	Key string
	// Title is the room's name as people see it.
	Title string
	// Speaker is the agent that speaks in the room.
	Speaker string
	// Invite lists the people let into the room.
	Invite []UserID
}

// Incoming is a message that someone sent to a room.
type Incoming struct {
	Room    RoomID
	Sender  UserID
	Message MessageID
	Kind    Kind
	// Body is the message's text as it was typed.
	Body string
	// Edit is true when the message replaces an earlier one.
	Edit bool
}

// Driver is the chat system as the city's server needs it. Its methods are
// safe for concurrent use.
type Driver interface {
	// EnsureRoom returns the room of the spec's Key, making it the first
	// time. It invites every user of the spec's Invite who is not in the
	// room yet, and leaves the others as they are.
	EnsureRoom(ctx context.Context, spec RoomSpec) (RoomID, error)
	// Send sends a message to a room as the speaker.
	Send(ctx context.Context, room RoomID, speaker string, msg Message) (MessageID, error)
	// React puts a reaction on a message as the speaker.
	React(ctx context.Context, room RoomID, speaker string, to MessageID, key string) error
	// Typing turns the speaker's typing notice in a room on or off.
	Typing(ctx context.Context, room RoomID, speaker string, on bool) error
	// Receive calls handle for each message that someone other than the
	// city's own speakers sends to a room of the city, until ctx ends.
	Receive(ctx context.Context, handle func(Incoming)) error
}

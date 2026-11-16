// Package session holds the server's registry of sessions and the seam the
// chat side works through.
package session

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned for a session the registry does not hold.
var ErrNotFound = errors.New("session: not found")

// ErrNotRunning is returned for an instruction to a session that has ended.
var ErrNotRunning = errors.New("session: not running")

// Kind says why a session exists.
type Kind int

const (
	// AlwaysOn is the session of an always-on agent.
	AlwaysOn Kind = iota + 1
	// Task is a session started for one task; it ends with the task.
	Task
)

// State is where a session is in its life.
type State int

const (
	// Starting is a session whose Job exists and whose host has not yet
	// attached and had the agent answer.
	Starting State = iota + 1
	Running
	Ended
)

// Info describes one session.
type Info struct {
	// Name is the session's name as gc gave it to the runtime.
	Name string
	// Agent is the name of the agent the session runs.
	Agent string
	Kind  Kind
	State State
	// Shown is the Seq of the last event of the session that the chat side
	// has shown; Service.Shown advances it. A start leaves it where it is:
	// it runs through the new start's own events as the chat side shows them.
	Shown uint64
}

// Direction says which way a frame went between the session host and the
// agent.
type Direction int

const (
	ToAgent Direction = iota + 1
	FromAgent
)

// Event is something that happened in a session: a Frame, a Prompt or a
// Lifecycle.
//
// Every event of a session has a Seq, one numbering for all three kinds,
// from 1 in the order the server recorded them. The numbering belongs to the
// session's name and goes on across a start afresh; a Lifecycle of kind
// StartedAfresh marks where the new agent's stream begins.
type Event interface {
	// Number returns the event's Seq.
	Number() uint64
}

// Frame is one JSON-RPC message of the ACP stream, as it was on the wire.
type Frame struct {
	Seq uint64
	// Time is when the frame passed the session host.
	Time      time.Time
	Direction Direction
	Message   []byte
}

// Source says whose prompt it is.
type Source int

const (
	// FromOwner is a prompt the owner gave through Service.Prompt.
	FromOwner Source = iota + 1
	// FromGC is gc's nudge.
	FromGC
)

// PromptState is where a prompt is on its way to the agent.
type PromptState int

const (
	// Stored is a prompt the server holds and has not yet sent on.
	Stored PromptState = iota + 1
	// Waiting is a prompt that cannot go into the agent's running turn and
	// waits for the turn's end. It is recorded when the server or the host
	// finds that it must wait, and not for a prompt that goes at once.
	Waiting
	// Delivered is a prompt the server handed to the session host; what the
	// host did with it is not reported back.
	Delivered
	// Dropped closes the record of a prompt of the owner's that waited and
	// was dropped by the owner's !stop: it will not be delivered. Its Of is
	// the Seq of the Stored record.
	Dropped
)

// Prompt records a prompt at each state it reaches, a Stored record first
// and a Delivered one last, so that the chat side can tell a prompt that
// waits from one the agent has taken. The frame that carries the prompt to
// the agent comes in the stream as well.
type Prompt struct {
	Seq uint64
	// Time is when the server recorded it.
	Time   time.Time
	Source Source
	Text   string
	State  PromptState
	// Of is, for a Waiting or Delivered record, the Seq of the Stored
	// record of the same prompt, which Service.Prompt returned for the
	// owner's; else 0.
	Of uint64
}

// LifecycleKind says what happened to a session.
type LifecycleKind int

const (
	// Started is the session's first start.
	Started LifecycleKind = iota + 1
	// StartedAfresh is a start after the agent's earlier session ended, with
	// nothing remembered.
	StartedAfresh
	// Stopped is the end of the session.
	Stopped
)

// Lifecycle is a change in the life of a session.
type Lifecycle struct {
	Seq uint64
	// Time is when the server recorded it.
	Time time.Time
	Kind LifecycleKind
}

func (e Frame) Number() uint64     { return e.Seq }
func (e Prompt) Number() uint64    { return e.Seq }
func (e Lifecycle) Number() uint64 { return e.Seq }

// Image is a picture sent with a prompt.
type Image struct {
	MIMEType string
	Data     []byte
}

// PromptInput is what the owner gives the agent.
type PromptInput struct {
	Text string
	// Images go to the agent only where it declares it takes them.
	Images []Image
}

// FromNow is the from of Subscribe that asks for no event already stored,
// only what happens after the call.
const FromNow = ^uint64(0)

// Service is what the chat side asks of the server's core. Its methods are
// safe for concurrent use, and each one that names a session returns
// ErrNotFound when the registry holds none of that name.
type Service interface {
	// List returns the sessions, those that have ended included.
	List(ctx context.Context) ([]Info, error)
	// Watch returns the sessions as they are now, one Info each, and then an
	// Info for each session that appears or changes state. The channel is
	// closed when ctx ends.
	Watch(ctx context.Context) (<-chan Info, error)
	// Subscribe returns the session's events with Seq greater than from
	// (none for FromNow), in order, then those that follow as they happen.
	// The channel is closed when ctx ends.
	Subscribe(ctx context.Context, name string, from uint64) (<-chan Event, error)
	// Prompt stores a prompt marked as the owner's and returns the Seq of
	// its Prompt event. It returns once the server has stored it, not once
	// the agent has taken it, so a session that is Starting accepts it. A
	// session that has ended gives ErrNotRunning, and one gc has not
	// started yet ErrNotFound.
	Prompt(ctx context.Context, name string, in PromptInput) (uint64, error)
	// Shown records that the events of the session up to seq have been shown:
	// Info.Shown advances to it, never back, and a smaller seq changes
	// nothing.
	Shown(ctx context.Context, name string, seq uint64) error
	// Starts returns the session's starts, the oldest first, and ErrNotFound
	// for a session that has none.
	Starts(ctx context.Context, name string) ([]Start, error)
	// SetRoom records the chat room of the session's current start.
	SetRoom(ctx context.Context, name, room string) error
	// Reset starts the session afresh: gc is asked to stop it and start it
	// again, which ends the session's current start as how "reset" and
	// begins the next. It returns ErrNotRunning for a session that has
	// ended, and an error when gc refuses.
	Reset(ctx context.Context, name string) error
	// Cancel is the server's whole share of !stop: it ends the running turn,
	// drops the owner's prompts that wait, each closed with a Dropped
	// record, keeps gc's nudges that wait and withdraws an open permission
	// request.
	Cancel(ctx context.Context, name string) error
	// AnswerPermission queues the answer to the agent's permission request
	// requestID: the option chosen, or "" to cancel the request.
	AnswerPermission(ctx context.Context, name, requestID, optionID string) error
}

// Authorizer checks the token a session sends with its calls.
type Authorizer interface {
	// Authorize reports whether token is the one the server made for the
	// session of that name.
	Authorize(name, token string) bool
}

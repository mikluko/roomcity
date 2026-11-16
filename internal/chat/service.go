package chat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"sarak.sh/mikluko/roomcity/internal/session"
)

// Service gives each always-on agent of the city a room.
type Service struct {
	Driver   Driver
	Sessions session.Service
	// Owners are the ids that hold the owner's role.
	Owners []UserID
	Log    *slog.Logger
	// RetryEvery is the pause before a room that could not be made is tried
	// again; zero means five seconds.
	RetryEvery time.Duration
	// PlanEvery is the shortest gap between two edits of the plan notice;
	// zero means 1.2 seconds.
	PlanEvery time.Duration
	// City is the city's name, which the space is called.
	City string
	// Transcripts is the address that a session's name is added to for the
	// link to its transcript; with none, the journal links no transcript.
	Transcripts string

	space, journal RoomID
	ready          chan struct{} // closed once the space and the journal are made
	tasks          map[string]*taskRoom
	known          map[string]RoomID // the open rooms in the space, by title; read once, kept up to date

	mu     sync.Mutex
	rooms  map[string]RoomID        // by agent
	by     map[RoomID]*agentSession // by room
	subs   map[string]struct{}      // sessions subscribed to
	making map[string]*sync.Mutex   // by agent: one room of an agent is made at a time
}

// agentSession is the always-on agent whose room it is and the session the
// owner's prompts go to.
type agentSession struct {
	agent   string
	session string
	room    RoomID
	// inputs carries what the room's people did that the renderer answers: a
	// reaction on a permission request.
	inputs chan Incoming

	mu       sync.Mutex
	messages map[uint64]chan MessageID // by the Seq of the prompt they became
}

// startedAfresh is the notice that goes to a room when the agent's session
// begins anew.
const startedAfresh = "The session began anew; nothing of the earlier one is remembered."

// notTaken is the notice for an owner's message that reached no session.
const notTaken = "The message was not taken: the session is not running."

// promptWait is how long a Waiting prompt waits to learn which message it came
// from.
const promptWait = 2 * time.Second

// Run makes the room of each always-on agent as its session appears, then
// carries messages between the rooms and the sessions until ctx ends. A room
// that exists is found again, so Run can be called again after a restart.
func (s *Service) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(ctx)
	defer func() {
		cancel()
		wg.Wait()
	}()

	s.rooms = map[string]RoomID{}
	s.by = map[RoomID]*agentSession{}
	s.subs = map[string]struct{}{}
	s.making = map[string]*sync.Mutex{}
	s.ready = make(chan struct{})
	s.tasks = map[string]*taskRoom{}
	wg.Go(func() { s.makeBase(ctx) })

	infos, err := s.Sessions.Watch(ctx)
	if err != nil {
		return fmt.Errorf("watching the sessions: %w", err)
	}
	received := make(chan error, 1)
	wg.Go(func() {
		if err := s.Driver.Receive(ctx, func(in Incoming) { s.receive(ctx, in) }); err != nil && ctx.Err() == nil {
			received <- fmt.Errorf("receiving: %w", err)
		}
	})
	for {
		select {
		case info, ok := <-infos:
			if !ok {
				if ctx.Err() != nil {
					return nil
				}
				return errors.New("the watch of the sessions ended")
			}
			wg.Go(func() { s.attach(ctx, &wg, info) })
		case err := <-received:
			return err
		case <-ctx.Done():
			return nil
		}
	}
}

// attach makes the agent's room if it has none and follows the session in it.
func (s *Service) attach(ctx context.Context, wg *sync.WaitGroup, info session.Info) {
	select {
	case <-s.ready:
	case <-ctx.Done():
		return
	}
	if info.Kind == session.Task {
		s.attachTask(ctx, wg, info)
		return
	}
	room, ok := s.room(ctx, info)
	if !ok {
		return
	}
	s.mu.Lock()
	target := s.by[room]
	if target == nil {
		target = &agentSession{
			agent:    info.Agent,
			room:     room,
			inputs:   make(chan Incoming, 64),
			messages: map[uint64]chan MessageID{},
		}
		s.by[room] = target
	}
	target.session = info.Name
	_, subscribed := s.subs[info.Name]
	s.subs[info.Name] = struct{}{}
	s.mu.Unlock()
	if subscribed {
		return
	}
	events, err := s.Sessions.Subscribe(ctx, info.Name, session.FromNow)
	if err != nil {
		s.Log.Error("subscribing to a session", "session", info.Name, "err", err)
		s.mu.Lock()
		delete(s.subs, info.Name)
		s.mu.Unlock()
		return
	}
	wg.Go(func() { s.render(ctx, target, info.Name, events, nil) })
}

// room returns the agent's room, making it if it has none. It reports false
// when ctx ended first.
func (s *Service) room(ctx context.Context, info session.Info) (RoomID, bool) {
	lock := s.agentLock(info.Agent)
	lock.Lock()
	defer lock.Unlock()
	s.mu.Lock()
	room, ok := s.rooms[info.Agent]
	s.mu.Unlock()
	if ok {
		return room, true
	}
	room, ok = s.ensure(ctx, RoomSpec{
		Key:     info.Agent,
		Title:   info.Agent,
		Speaker: info.Agent,
		Invite:  s.Owners,
		Space:   s.space,
	})
	if !ok {
		return "", false
	}
	s.mu.Lock()
	s.rooms[info.Agent] = room
	s.mu.Unlock()
	return room, true
}

func (s *Service) agentLock(agent string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.making[agent] == nil {
		s.making[agent] = &sync.Mutex{}
	}
	return s.making[agent]
}

// render follows one session's events and the room's inputs until the events
// end, or, when onStopped is set, until the session's own end, after which it
// calls onStopped.
func (s *Service) render(ctx context.Context, t *agentSession, name string, events <-chan session.Event, onStopped func()) {
	r := newRenderer(&roomOutbox{s: s, ctx: ctx, t: t, session: name}, time.Now, s.PlanEvery)
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return
			}
			s.journalAfresh(ctx, t, ev)
			r.event(ev)
			if l, ok := ev.(session.Lifecycle); ok && l.Kind == session.Stopped && onStopped != nil {
				onStopped()
				return
			}
		case in := <-t.inputs:
			if in.Kind == Reaction {
				r.answerPermission(in.To, in.Key, in.Sender)
			} else {
				r.stopped()
			}
		case <-tick.C:
			r.tick()
		}
	}
}

// journalAfresh tells the journal that an always-on agent's session began
// anew, with a link to its room.
func (s *Service) journalAfresh(ctx context.Context, t *agentSession, ev session.Event) {
	if l, ok := ev.(session.Lifecycle); ok && l.Kind == session.StartedAfresh {
		s.journalNotice(ctx, fmt.Sprintf("The session of **%s** began anew: [%s](%s)", t.agent, t.agent, s.Driver.Link(t.room)))
	}
}

// roomOutbox is what a renderer does to an agent's room.
type roomOutbox struct {
	s       *Service
	ctx     context.Context
	t       *agentSession
	session string
}

func (o *roomOutbox) send(m Message) MessageID {
	id, err := o.s.Driver.Send(o.ctx, o.t.room, o.t.agent, m)
	if err != nil {
		o.s.Log.Error("sending to a room", "agent", o.t.agent, "err", err)
	}
	return id
}

func (o *roomOutbox) react(to MessageID, key string) ReactionID {
	id, err := o.s.Driver.React(o.ctx, o.t.room, o.t.agent, to, key)
	if err != nil {
		o.s.Log.Error("reacting in a room", "agent", o.t.agent, "err", err)
	}
	return id
}

func (o *roomOutbox) unreact(r ReactionID) {
	if err := o.s.Driver.Unreact(o.ctx, o.t.room, o.t.agent, r); err != nil {
		o.s.Log.Error("taking a reaction back", "agent", o.t.agent, "err", err)
	}
}

func (o *roomOutbox) typing(on bool) {
	if err := o.s.Driver.Typing(o.ctx, o.t.room, o.t.agent, on); err != nil {
		o.s.Log.Debug("typing notice", "agent", o.t.agent, "err", err)
	}
}

func (o *roomOutbox) promptMessage(seq uint64) MessageID {
	select {
	case id := <-o.t.slot(seq):
		return id
	case <-time.After(promptWait):
		return ""
	case <-o.ctx.Done():
		return ""
	}
}

func (o *roomOutbox) forgetPrompt(seq uint64) {
	o.t.mu.Lock()
	defer o.t.mu.Unlock()
	delete(o.t.messages, seq)
}

func (o *roomOutbox) answerPermission(requestID, optionID string) {
	if err := o.s.Sessions.AnswerPermission(o.ctx, o.session, requestID, optionID); err != nil {
		o.s.Log.Error("answering a permission request", "agent", o.t.agent, "err", err)
	}
}

// slot returns the channel on which the message that became the prompt of that
// Seq is made known. The channel holds one value and is read once.
func (t *agentSession) slot(seq uint64) chan MessageID {
	t.mu.Lock()
	defer t.mu.Unlock()
	ch, ok := t.messages[seq]
	if !ok {
		ch = make(chan MessageID, 1)
		t.messages[seq] = ch
	}
	return ch
}

// receive handles what someone did in a room: a message that may be an
// owner's prompt or command, a reaction that may answer a permission request.
func (s *Service) receive(ctx context.Context, in Incoming) {
	s.mu.Lock()
	target, ok := s.by[in.Room]
	var current string
	if ok {
		current = target.session
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	switch {
	case !slices.Contains(s.Owners, in.Sender):
		s.Log.Warn("ignoring someone without the owner's role", "agent", target.agent, "sender", in.Sender)
	case in.Kind == Reaction:
		select {
		case target.inputs <- in:
		default:
			s.Log.Warn("dropping a reaction: the room is behind", "agent", target.agent)
		}
	case in.Edit:
		s.Log.Debug("ignoring an edit: what the agent received stays received", "agent", target.agent)
	case in.Kind != Text:
		s.Log.Debug("ignoring a notice", "agent", target.agent)
	case strings.HasPrefix(in.Body, "!"):
		s.command(ctx, target, current, in)
	default:
		s.prompt(ctx, target, current, in)
	}
}

func (s *Service) prompt(ctx context.Context, t *agentSession, name string, in Incoming) {
	seq, err := s.Sessions.Prompt(ctx, name, session.PromptInput{Text: in.Body})
	switch {
	case err == nil:
		t.slot(seq) <- in.Message
	case errors.Is(err, session.ErrNotFound), errors.Is(err, session.ErrNotRunning):
		s.Log.Warn("dropping a prompt: the session is not there", "agent", t.agent, "err", err)
		s.notice(ctx, t, notTaken, in.Message)
	default:
		s.Log.Error("giving the prompt to the agent", "agent", t.agent, "err", err)
	}
}

func (s *Service) notice(ctx context.Context, t *agentSession, body string, replyTo MessageID) {
	if _, err := s.Driver.Send(ctx, t.room, t.agent, Message{Kind: Notice, Body: body, ReplyTo: replyTo}); err != nil {
		s.Log.Error("sending a notice", "agent", t.agent, "err", err)
	}
}

// commands are the messages that begin with "!" and that the runtime answers
// itself; the agent never sees one.
const helpText = "Commands: `!stop` ends the running turn and drops your messages that wait, while gc's nudges stay and are delivered after it; `!help` lists them."

func (s *Service) command(ctx context.Context, t *agentSession, name string, in Incoming) {
	word := strings.Fields(in.Body)[0]
	switch word {
	case "!stop":
		if err := s.Sessions.Cancel(ctx, name); err != nil {
			s.Log.Error("cancelling a turn", "agent", t.agent, "err", err)
			s.notice(ctx, t, "Could not stop: "+err.Error(), in.Message)
			return
		}
		select {
		case t.inputs <- Incoming{Kind: Text, Body: word}:
		default:
		}
		s.notice(ctx, t, "Asked the agent to stop: your waiting messages are dropped, gc's nudges stay.", in.Message)
	case "!help":
		s.notice(ctx, t, helpText, in.Message)
	default:
		s.notice(ctx, t, fmt.Sprintf("Unknown command `%s`. %s", word, helpText), in.Message)
	}
}

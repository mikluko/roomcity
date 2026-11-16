package chat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
	// CatchUp is how old an event may be and still be shown when a room is
	// brought up to date; zero means DefaultCatchUp.
	CatchUp time.Duration
	// MaxFile and FileKinds bound what Attach sends: zero and none are
	// DefaultMaxFile and DefaultFileKinds.
	MaxFile   Size
	FileKinds []string

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
	s.recordRoom(ctx, target, info.Name)
	wg.Go(func() { s.follow(ctx, target, info.Name, info.Shown, nil) })
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

// follow shows a session in its room from the event after from until ctx ends
// or, when onStopped is set, until the session's own end, after which it calls
// onStopped. What cannot be sent to the room is not given up: the session's
// stream is read again from the last event that was shown, and the room is
// brought up to date from there.
//
// A room that stays out of reach is told of once when it goes, once a minute
// while it stays, and once when it is back.
func (s *Service) follow(ctx context.Context, t *agentSession, name string, from uint64, onStopped func()) {
	cursor := from
	var out outage
	for ctx.Err() == nil {
		a := s.show(ctx, t, name, cursor, onStopped)
		cursor = a.cursor
		out.note(s.Log, name, t.agent, a, time.Now())
		if !a.again {
			return
		}
		select {
		case <-time.After(s.retryEvery()):
		case <-ctx.Done():
			return
		}
	}
}

// attempt is what one reading of a session's stream came to: the last event
// shown and kept, whether to read again from there, the first thing the room
// did not take, and how many messages and reactions it did.
type attempt struct {
	cursor uint64
	again  bool
	err    error
	sent   int64
}

// outage is a room's time out of reach, as it is logged.
type outage struct {
	since, logged time.Time
}

// logEvery is the shortest gap between two lines about a room that stays out
// of reach.
const logEvery = time.Minute

// note logs what the attempt shows of the room: the first failure, a line a
// minute for as long as it fails, and the recovery.
func (o *outage) note(log *slog.Logger, name, agent string, a attempt, now time.Time) {
	if !o.since.IsZero() && a.sent > 0 {
		log.Info("a room takes what is sent to it again", "session", name, "agent", agent, "out_for", now.Sub(o.since).Round(time.Second))
		*o = outage{}
	}
	if a.err == nil {
		return
	}
	switch {
	case o.since.IsZero():
		*o = outage{since: now, logged: now}
		log.Error("a room does not take what is sent to it; the session is read again from where it was shown", "session", name, "agent", agent, "err", a.err)
	case now.Sub(o.logged) >= logEvery:
		o.logged = now
		log.Warn("a room still does not take what is sent to it", "session", name, "agent", agent, "out_for", now.Sub(o.since).Round(time.Second), "err", a.err)
	}
}

// saveEvery is the shortest gap between two records of how far a session has
// been shown.
const saveEvery = 200 * time.Millisecond

// catchUpPause is how long the events of a replay must stay out before the
// room is told that part of it was left out.
const catchUpPause = 500 * time.Millisecond

// show reads the session's events after from and shows them. It returns the
// last event that is shown and kept, and whether to read again from there: the
// room did not take what was sent, or the stream ended without ctx.
func (s *Service) show(ctx context.Context, t *agentSession, name string, from uint64, onStopped func()) attempt {
	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	events, err := s.Sessions.Subscribe(subCtx, name, from)
	if errors.Is(err, session.ErrNotFound) {
		return attempt{cursor: from}
	}
	if err != nil {
		return attempt{cursor: from, again: true, err: fmt.Errorf("subscribing to the session: %w", err)}
	}
	out := &roomOutbox{s: s, ctx: ctx, t: t, session: name}
	w := &showing{
		s: s, ctx: ctx, t: t, name: name, onStopped: onStopped,
		out:  out,
		r:    newRenderer(out, time.Now, s.PlanEvery),
		pos:  position{shown: from, kept: from, saved: from},
		skip: replay{cutoff: time.Now().Add(-s.catchUp())},
	}
	tick := time.NewTicker(saveEvery)
	defer tick.Stop()
	defer w.save()
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return w.result(ctx.Err() == nil)
			}
			if done, again := w.event(ev); done {
				return w.result(again)
			}
		case in := <-t.inputs:
			if in.Kind == Reaction {
				w.r.answerPermission(in.To, in.Key, in.Sender)
			} else {
				w.r.stopped()
			}
		case <-tick.C:
			if again := w.tick(); again {
				return w.result(true)
			}
		}
	}
}

// noteStart records the room of the start that a Lifecycle event opens, and
// for a start afresh tells the journal, with a link to the room and to the
// transcript of the start that just closed.
func (s *Service) noteStart(ctx context.Context, t *agentSession, name string, ev session.Event) {
	l, ok := ev.(session.Lifecycle)
	if !ok || (l.Kind != session.Started && l.Kind != session.StartedAfresh) {
		return
	}
	s.recordRoom(ctx, t, name)
	if l.Kind != session.StartedAfresh {
		return
	}
	body := fmt.Sprintf("The session of **%s** began anew: [%s](%s)", t.agent, t.agent, s.Driver.Link(t.room))
	if closed, ok := s.closedBefore(ctx, name, l.Seq); ok && s.Transcripts != "" {
		body += fmt.Sprintf(" · [the transcript of the one that ended](%s%s/%d)", s.Transcripts, url.PathEscape(name), closed)
	}
	s.journalNotice(ctx, body)
}

// closedBefore returns the number of the start before the one that the event
// of that Seq opened.
func (s *Service) closedBefore(ctx context.Context, name string, seq uint64) (int, bool) {
	starts, err := s.Sessions.Starts(ctx, name)
	if err != nil {
		s.Log.Error("reading the starts of a session", "session", name, "err", err)
		return 0, false
	}
	i := slices.IndexFunc(starts, func(st session.Start) bool { return st.FirstSeq == seq })
	if i < 1 {
		return 0, false
	}
	return starts[i-1].N, true
}

// recordRoom tells the registry which room the session's current start is
// shown in.
func (s *Service) recordRoom(ctx context.Context, t *agentSession, name string) {
	if err := s.Sessions.SetRoom(ctx, name, string(t.room)); err != nil && !errors.Is(err, session.ErrNotFound) {
		s.Log.Error("recording the room of a session's start", "session", name, "err", err)
	}
}

// showing is one reading of a session's stream into its room.
type showing struct {
	s         *Service
	ctx       context.Context
	t         *agentSession
	name      string
	onStopped func()
	out       *roomOutbox
	r         *renderer
	pos       position
	skip      replay
}

// result is what the reading came to.
func (w *showing) result(again bool) attempt {
	return attempt{cursor: w.pos.kept, again: again, err: w.out.err(), sent: w.out.delivered.Load()}
}

// event shows one event. It reports whether the reading is over, and then
// whether to read again.
func (w *showing) event(ev session.Event) (done, again bool) {
	if w.skip.leaves(ev) {
		return false, false
	}
	if !w.noteLeftOut() {
		return true, true
	}
	w.r.event(ev)
	if w.out.failed() {
		return true, true
	}
	// Only once the room has the event: the journal's notice goes by a send
	// of its own that is not counted, and would be written at each reading.
	w.s.noteStart(w.ctx, w.t, w.name, ev)
	w.pos.shown = ev.Number()
	w.keep()
	if l, ok := ev.(session.Lifecycle); ok && l.Kind == session.Stopped && w.onStopped != nil {
		w.pos.kept = w.pos.shown
		w.save()
		w.onStopped()
		return true, false
	}
	return false, false
}

// tick does what is due with time, and records how far the session is shown.
// It reports whether to read again.
func (w *showing) tick() bool {
	w.r.tick()
	if w.out.failed() {
		return true
	}
	if w.skip.due(time.Now()) && !w.noteLeftOut() {
		return true
	}
	w.keep()
	w.save()
	return false
}

// keep moves the kept position to the last event after which nothing is held
// back unsent, as the renderer says.
func (w *showing) keep() {
	w.pos.kept = max(w.pos.kept, w.r.safe(w.pos.shown))
}

// save records how far the session is shown, if that has moved.
func (w *showing) save() {
	if w.pos.kept <= w.pos.saved {
		return
	}
	err := w.s.Sessions.Shown(context.WithoutCancel(w.ctx), w.name, w.pos.kept)
	if err != nil && !errors.Is(err, session.ErrNotFound) {
		w.s.Log.Error("recording how far a session is shown", "session", w.name, "err", err)
		return
	}
	w.pos.saved = w.pos.kept
}

// noteLeftOut tells the room that events were left out, once, before the first
// that is shown, and counts them as shown only when the room has the notice.
// It reports false when the room did not take it.
func (w *showing) noteLeftOut() bool {
	if w.skip.left == 0 {
		return true
	}
	body := fmt.Sprintf("What happened before %s is not shown here: it is older than the %s that the room catches up on after an outage.",
		w.skip.cutoff.UTC().Format("15:04Z"), w.s.catchUp())
	if w.s.Transcripts != "" {
		body += fmt.Sprintf(" [Transcript](%s%s)", w.s.Transcripts, url.PathEscape(w.name))
	}
	w.r.send(Message{Kind: Notice, Body: body})
	if w.out.failed() {
		return false
	}
	w.pos.shown, w.pos.kept = w.skip.left, w.skip.left
	w.skip.left = 0
	return true
}

// position is how far a session has been shown: the last event handled, the
// last at which nothing waited unsent, and the last that is recorded.
type position struct{ shown, kept, saved uint64 }

// replay finds the events of a stream that are older than the room catches up
// on.
type replay struct {
	cutoff time.Time
	// left is the last event left out, and at when; nothing is left out
	// when it is zero.
	left uint64
	at   time.Time
}

// leaves says whether the event is too old to be shown, and takes note of it.
func (p *replay) leaves(ev session.Event) bool {
	at, ok := eventTime(ev)
	if !ok || !at.Before(p.cutoff) {
		return false
	}
	p.left = ev.Number()
	p.at = time.Now()
	return true
}

// due says whether events were left out and no more has come for a while.
func (p *replay) due(now time.Time) bool {
	return p.left != 0 && now.Sub(p.at) >= catchUpPause
}

// eventTime returns when the server recorded the event.
func eventTime(ev session.Event) (time.Time, bool) {
	var at time.Time
	switch ev := ev.(type) {
	case session.Frame:
		at = ev.Time
	case session.Prompt:
		at = ev.Time
	case session.Lifecycle:
		at = ev.Time
	}
	return at, !at.IsZero()
}

func (s *Service) catchUp() time.Duration {
	if s.CatchUp == 0 {
		return DefaultCatchUp
	}
	return s.CatchUp
}

func (s *Service) retryEvery() time.Duration {
	if s.RetryEvery > 0 {
		return s.RetryEvery
	}
	return 5 * time.Second
}

// roomOutbox is what a renderer does to an agent's room.
type roomOutbox struct {
	s       *Service
	ctx     context.Context
	t       *agentSession
	session string
	// lost holds the first message or reaction that did not reach the room:
	// what was shown from there on is not to be kept. The reading's own
	// loop logs it, since it is read again.
	lost atomic.Pointer[error]
	// delivered counts what the room took.
	delivered atomic.Int64
}

// failed says whether the room has not taken a message or a reaction.
func (o *roomOutbox) failed() bool { return o.lost.Load() != nil }

// err returns the first thing the room did not take, or nil.
func (o *roomOutbox) err() error {
	if p := o.lost.Load(); p != nil {
		return *p
	}
	return nil
}

func (o *roomOutbox) lose(err error) { o.lost.CompareAndSwap(nil, &err) }

func (o *roomOutbox) send(m Message) MessageID {
	id, err := o.s.Driver.Send(o.ctx, o.t.room, o.t.agent, m)
	if o.took(err, "sending to a room") {
		return id
	}
	return ""
}

func (o *roomOutbox) react(to MessageID, key string) ReactionID {
	id, err := o.s.Driver.React(o.ctx, o.t.room, o.t.agent, to, key)
	if o.took(err, "reacting in a room") {
		return id
	}
	return ""
}

// took says what became of a call to the room. A call the chat system refused
// is logged and left behind: asking again would be refused again, and the
// stream goes on. Any other failure is the room being out of reach, and ends
// the reading, which is made again from the last position kept.
func (o *roomOutbox) took(err error, what string) bool {
	switch {
	case err == nil:
		o.delivered.Add(1)
		return true
	case IsRefused(err):
		o.s.Log.Warn(what+": the chat system refused it, and it is left out", "agent", o.t.agent, "err", err)
	default:
		o.lose(fmt.Errorf("%s: %w", what, err))
	}
	return false
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
const helpText = "Commands: `!stop` ends the running turn and drops your messages that wait, while gc's nudges stay and are delivered after it; `!reset` starts the session afresh, and the agent remembers nothing of the one it ends; `!help` lists them."

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
	case "!reset":
		if err := s.Sessions.Reset(ctx, name); err != nil {
			s.Log.Error("resetting a session", "agent", t.agent, "err", err)
			s.notice(ctx, t, "Could not reset: "+err.Error(), in.Message)
			return
		}
		s.notice(ctx, t, "Asked gc to start the session afresh: it stops and starts again, and the room says when it has begun anew.", in.Message)
	case "!help":
		s.notice(ctx, t, helpText, in.Message)
	default:
		s.notice(ctx, t, fmt.Sprintf("Unknown command `%s`. %s", word, helpText), in.Message)
	}
}

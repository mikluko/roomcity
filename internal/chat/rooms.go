package chat

import (
	"context"
	"fmt"
	"net/url"
	"sync"
	"time"

	"sarak.sh/mikluko/roomcity/internal/session"
)

// ensure makes the room of the spec, or finds it, and tries again after a
// pause for as long as the chat system fails. It reports false when ctx ended
// first.
func (s *Service) ensure(ctx context.Context, spec RoomSpec) (RoomID, bool) {
	pause := s.RetryEvery
	if pause == 0 {
		pause = 5 * time.Second
	}
	for {
		room, err := s.Driver.EnsureRoom(ctx, spec)
		if err == nil {
			return room, true
		}
		s.Log.Error("making a room, trying again", "room", spec.Title, "err", err)
		select {
		case <-time.After(pause):
		case <-ctx.Done():
			return "", false
		}
	}
}

// makeBase makes the city's space, with the owners invited, and the journal in
// it, and then lets the rooms of the sessions be made.
func (s *Service) makeBase(ctx context.Context) {
	space, ok := s.ensure(ctx, RoomSpec{Title: s.City, IsSpace: true, Speaker: "", Invite: s.Owners})
	if !ok {
		return
	}
	journal, ok := s.ensure(ctx, RoomSpec{Key: "journal", Title: "journal", Invite: s.Owners, Space: space})
	if !ok {
		return
	}
	known, ok := s.readKnown(ctx, space)
	if !ok {
		return
	}
	s.prune(ctx)
	s.mu.Lock()
	s.space, s.journal, s.known = space, journal, known
	s.mu.Unlock()
	close(s.ready)
}

// prune removes from the city's rooms those who no longer hold a role. With no
// owner in the configuration it removes no one: that is a mistake, not a wish
// to empty the city.
func (s *Service) prune(ctx context.Context) {
	if len(s.Owners) == 0 {
		s.Log.Warn("no one holds the owner's role, so no one is removed from the rooms")
		return
	}
	if err := s.Driver.Prune(ctx, s.Owners); err != nil {
		s.Log.Error("removing those who hold no role from the rooms", "err", err)
	}
}

// readKnown reads the open rooms of the space, once, and tries again after a
// pause while the chat system fails.
func (s *Service) readKnown(ctx context.Context, space RoomID) (map[string]RoomID, bool) {
	pause := s.RetryEvery
	if pause == 0 {
		pause = 5 * time.Second
	}
	for {
		known, err := s.Driver.OpenRooms(ctx, space)
		if err == nil {
			return known, true
		}
		s.Log.Error("reading the rooms of the space, trying again", "err", err)
		select {
		case <-time.After(pause):
		case <-ctx.Done():
			return nil, false
		}
	}
}

// journalNotice adds a line to the journal.
func (s *Service) journalNotice(ctx context.Context, body string) {
	if _, err := s.Driver.Send(ctx, s.journal, "", Message{Kind: Notice, Body: body}); err != nil {
		s.Log.Error("writing to the journal", "err", err)
	}
}

// taskRoom is the room of a session started for a task.
type taskRoom struct {
	target *agentSession
	cancel context.CancelFunc
}

// taskSpec is the room of a task's session: named for the agent and the
// session, in the space, and open to its members.
func (s *Service) taskSpec(info session.Info) RoomSpec {
	return RoomSpec{Title: info.Agent + " " + info.Name, Speaker: info.Agent, Space: s.space, Open: true}
}

// attachTask follows a task's session in a room of its own while it runs. The
// room leaves the space when the session's own stream ends, so that what the
// agent said last is shown first. A session that ended while nobody watched
// is settled here, if its room is still in the space.
func (s *Service) attachTask(ctx context.Context, wg *sync.WaitGroup, info session.Info) {
	lock := s.agentLock("task " + info.Name)
	lock.Lock()
	defer lock.Unlock()
	s.mu.Lock()
	task := s.tasks[info.Name]
	s.mu.Unlock()
	switch {
	case task != nil:
	case info.State != session.Ended:
		s.startTask(ctx, wg, info)
	default:
		s.settleEnded(ctx, info)
	}
}

func (s *Service) startTask(ctx context.Context, wg *sync.WaitGroup, info session.Info) {
	spec := s.taskSpec(info)
	s.mu.Lock()
	existing := s.known[spec.Title]
	s.mu.Unlock()
	room, ok := s.ensure(ctx, spec)
	if !ok {
		return
	}
	s.mu.Lock()
	s.known[spec.Title] = room
	s.mu.Unlock()
	taskCtx, cancel := context.WithCancel(ctx)
	task := &taskRoom{
		cancel: cancel,
		target: &agentSession{
			agent:    info.Agent,
			session:  info.Name,
			room:     room,
			inputs:   make(chan Incoming, 64),
			messages: map[uint64]chan MessageID{},
		},
	}
	s.mu.Lock()
	s.tasks[info.Name] = task
	s.by[room] = task.target
	s.mu.Unlock()

	events, err := s.Sessions.Subscribe(taskCtx, info.Name, session.FromNow)
	if err != nil {
		s.Log.Error("subscribing to a session", "session", info.Name, "err", err)
		cancel()
		s.mu.Lock()
		delete(s.by, room)
		delete(s.tasks, info.Name)
		s.mu.Unlock()
		return
	}
	wg.Go(func() {
		s.render(taskCtx, task.target, info.Name, events, func() { s.endTask(ctx, info, task) })
	})
	if existing == "" {
		s.journalNotice(ctx, fmt.Sprintf("A session of **%s** started: [%s](%s)", info.Agent, spec.Title, s.Driver.Link(room)))
	}
}

// endTask takes a task's room out of the space once its session has ended.
func (s *Service) endTask(ctx context.Context, info session.Info, task *taskRoom) {
	task.cancel()
	s.mu.Lock()
	delete(s.by, task.target.room)
	delete(s.tasks, info.Name)
	s.mu.Unlock()
	s.retireTask(ctx, info, task.target.room)
}

// settleEnded takes the room of a task whose session ended while the server
// was away out of the space, if it is still there, and writes the journal's
// line for the end.
func (s *Service) settleEnded(ctx context.Context, info session.Info) {
	s.mu.Lock()
	room := s.known[s.taskSpec(info).Title]
	s.mu.Unlock()
	if room != "" {
		s.retireTask(ctx, info, room)
	}
}

func (s *Service) retireTask(ctx context.Context, info session.Info, room RoomID) {
	s.mu.Lock()
	delete(s.known, s.taskSpec(info).Title)
	s.mu.Unlock()
	if err := s.Driver.Retire(ctx, room, info.Agent); err != nil {
		s.Log.Error("taking a task's room out of the space", "session", info.Name, "err", err)
	}
	body := fmt.Sprintf("The session of **%s**, %s, ended.", info.Agent, info.Name)
	if s.Transcripts != "" {
		body += fmt.Sprintf(" [Transcript](%s%s)", s.Transcripts, url.PathEscape(info.Name))
	}
	s.journalNotice(ctx, body)
}

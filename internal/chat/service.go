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

	mu     sync.Mutex
	making map[string]*sync.Mutex   // by agent: one room of an agent is made at a time
	rooms  map[string]RoomID        // by agent
	by     map[RoomID]*agentSession // by room
	subs   map[string]struct{}      // sessions subscribed to
}

// agentSession is the always-on agent whose room it is and the session the
// owner's prompts go to.
type agentSession struct {
	agent   string
	session string
}

// startedAfresh is the notice that goes to a room when the agent's session
// begins anew.
const startedAfresh = "The session began anew; nothing of the earlier one is remembered."

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
			if info.Kind == session.AlwaysOn {
				wg.Go(func() { s.attach(ctx, &wg, info) })
			}
		case err := <-received:
			return err
		case <-ctx.Done():
			return nil
		}
	}
}

// attach makes the agent's room if it has none and follows the session in it.
func (s *Service) attach(ctx context.Context, wg *sync.WaitGroup, info session.Info) {
	room, ok := s.room(ctx, info)
	if !ok {
		return
	}
	s.mu.Lock()
	s.by[room] = &agentSession{agent: info.Agent, session: info.Name}
	_, subscribed := s.subs[info.Name]
	if info.State != session.Ended {
		s.subs[info.Name] = struct{}{}
	}
	s.mu.Unlock()
	if subscribed || info.State == session.Ended {
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
	wg.Go(func() { s.render(ctx, info.Agent, room, events) })
}

// room returns the agent's room, making it if it has none, and tries again
// after a pause for as long as the chat system fails. It reports false when
// ctx ended first.
func (s *Service) room(ctx context.Context, info session.Info) (RoomID, bool) {
	lock := s.agentLock(info.Agent)
	lock.Lock()
	defer lock.Unlock()
	pause := s.RetryEvery
	if pause == 0 {
		pause = 5 * time.Second
	}
	for {
		s.mu.Lock()
		room, ok := s.rooms[info.Agent]
		s.mu.Unlock()
		if ok {
			return room, true
		}
		room, err := s.Driver.EnsureRoom(ctx, RoomSpec{
			Key:     info.Agent,
			Title:   info.Agent,
			Speaker: info.Agent,
			Invite:  s.Owners,
		})
		if err == nil {
			s.mu.Lock()
			s.rooms[info.Agent] = room
			s.mu.Unlock()
			return room, true
		}
		s.Log.Error("making the room of an agent, trying again", "agent", info.Agent, "err", err)
		select {
		case <-time.After(pause):
		case <-ctx.Done():
			return "", false
		}
	}
}

func (s *Service) agentLock(agent string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.making[agent] == nil {
		s.making[agent] = &sync.Mutex{}
	}
	return s.making[agent]
}

func (s *Service) render(ctx context.Context, agent string, room RoomID, events <-chan session.Event) {
	var a answer
	for ev := range events {
		for _, text := range a.feed(ev) {
			s.send(ctx, agent, room, Message{Kind: Text, Body: text})
		}
		if l, ok := ev.(session.Lifecycle); ok && l.Kind == session.StartedAfresh {
			s.send(ctx, agent, room, Message{Kind: Notice, Body: startedAfresh})
		}
	}
}

func (s *Service) send(ctx context.Context, agent string, room RoomID, msg Message) {
	if _, err := s.Driver.Send(ctx, room, agent, msg); err != nil {
		s.Log.Error("sending to a room", "agent", agent, "err", err)
	}
}

// receive turns a message in an agent's room into a prompt when an owner
// wrote it.
func (s *Service) receive(ctx context.Context, in Incoming) {
	s.mu.Lock()
	target, ok := s.by[in.Room]
	s.mu.Unlock()
	if !ok {
		return
	}
	switch {
	case !slices.Contains(s.Owners, in.Sender):
		s.Log.Warn("ignoring a message of someone without the owner's role", "agent", target.agent, "sender", in.Sender)
	case in.Edit:
		s.Log.Debug("ignoring an edit: what the agent received stays received", "agent", target.agent)
	case in.Kind != Text:
		s.Log.Debug("ignoring a notice", "agent", target.agent)
	case strings.HasPrefix(in.Body, "!"):
		s.Log.Info("a command of the runtime, not handled here", "agent", target.agent, "sender", in.Sender)
	default:
		_, err := s.Sessions.Prompt(ctx, target.session, session.PromptInput{Text: in.Body})
		if errors.Is(err, session.ErrNotFound) || errors.Is(err, session.ErrNotRunning) {
			s.Log.Warn("dropping a prompt: the session is not there", "agent", target.agent, "err", err)
		} else if err != nil {
			s.Log.Error("giving the prompt to the agent", "agent", target.agent, "err", err)
		}
	}
}

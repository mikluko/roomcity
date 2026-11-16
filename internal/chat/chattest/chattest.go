// Package chattest holds what tests of the chat side stand on: a session
// service that does what the test says, and a recorded ACP session.
package chattest

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sync"

	"sarak.sh/mikluko/roomcity/internal/session"
)

// Sessions is a session.Service with the sessions a test adds and the events
// it emits. Only what the chat side calls is implemented: List, Watch,
// Subscribe, Prompt, Cancel and AnswerPermission, which only record.
type Sessions struct {
	mu         sync.Mutex
	infos      []session.Info
	watch      chan session.Info
	closeWatch func()
	events     map[string]chan session.Event
	prompts    map[string][]string
	cancels    map[string]int
	answers    []PermissionAnswer
	subscribed map[string]int
	shown      map[string]uint64
	resets     []string

	// PromptErr, when set, is what Prompt returns, and the prompt is not kept.
	PromptErr error
	// ResetErr, when set, is what Reset returns, and the request is not kept.
	ResetErr error
}

// PermissionAnswer is a call of AnswerPermission.
type PermissionAnswer struct {
	Session, RequestID, OptionID string
}

var _ session.Service = (*Sessions)(nil)

// Add makes a session appear, or change state, for those who Watch.
func (s *Sessions) Add(info session.Info) {
	s.mu.Lock()
	replaced := false
	for i := range s.infos {
		if s.infos[i].Name == info.Name {
			s.infos[i], replaced = info, true
		}
	}
	if !replaced {
		s.infos = append(s.infos, info)
	}
	watch := s.watch
	s.mu.Unlock()
	if watch != nil {
		watch <- info
	}
}

// List implements session.Service.
func (s *Sessions) List(context.Context) ([]session.Info, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]session.Info(nil), s.infos...), nil
}

// Watch implements session.Service.
func (s *Sessions) Watch(ctx context.Context) (<-chan session.Info, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := make(chan session.Info, 64)
	for _, info := range s.infos {
		ch <- info
	}
	s.watch = ch
	var once sync.Once
	closeCh := func() {
		once.Do(func() {
			s.mu.Lock()
			s.watch = nil
			s.mu.Unlock()
			close(ch)
		})
	}
	s.closeWatch = closeCh
	go func() {
		<-ctx.Done()
		closeCh()
	}()
	return ch, nil
}

// CloseWatch ends the Watch channel while its context is live, as a registry
// that gave up would.
func (s *Sessions) CloseWatch() {
	s.mu.Lock()
	closeCh := s.closeWatch
	s.mu.Unlock()
	closeCh()
}

// Subscribe implements session.Service. It serves only the events emitted
// after the call, whatever from says.
func (s *Sessions) Subscribe(ctx context.Context, name string, _ uint64) (<-chan session.Event, error) {
	ch := s.channel(name)
	s.mu.Lock()
	if s.subscribed == nil {
		s.subscribed = map[string]int{}
	}
	s.subscribed[name]++
	s.mu.Unlock()
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

// Prompt implements session.Service.
func (s *Sessions) Prompt(_ context.Context, name string, in session.PromptInput) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.PromptErr != nil {
		return 0, s.PromptErr
	}
	if s.prompts == nil {
		s.prompts = map[string][]string{}
	}
	s.prompts[name] = append(s.prompts[name], in.Text)
	return uint64(len(s.prompts[name])), nil
}

// Cancel implements session.Service.
func (s *Sessions) Cancel(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancels == nil {
		s.cancels = map[string]int{}
	}
	s.cancels[name]++
	return nil
}

// Cancels returns how many times the session was cancelled.
func (s *Sessions) Cancels(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cancels[name]
}

// Shown implements session.Service: it records the highest seq it was given
// for the session, as Info.Shown, and moves nothing back.
func (s *Sessions) Shown(_ context.Context, name string, seq uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.shown == nil {
		s.shown = map[string]uint64{}
	}
	if seq > s.shown[name] {
		s.shown[name] = seq
	}
	return nil
}

// ShownSeq returns the highest seq the session was said to have shown.
func (s *Sessions) ShownSeq(name string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shown[name]
}

// Reset implements session.Service: it records the request.
func (s *Sessions) Reset(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ResetErr != nil {
		return s.ResetErr
	}
	s.resets = append(s.resets, name)
	return nil
}

// Resets returns the names of the sessions that were reset, in order.
func (s *Sessions) Resets() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.resets)
}

// AnswerPermission implements session.Service.
func (s *Sessions) AnswerPermission(_ context.Context, name, requestID, optionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.answers = append(s.answers, PermissionAnswer{name, requestID, optionID})
	return nil
}

// Answers returns the permission answers given so far.
func (s *Sessions) Answers() []PermissionAnswer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]PermissionAnswer(nil), s.answers...)
}

// Emit makes the session produce events.
func (s *Sessions) Emit(name string, events ...session.Event) {
	ch := s.channel(name)
	for _, ev := range events {
		ch <- ev
	}
}

// Subscriptions returns how many times the session has been subscribed to.
func (s *Sessions) Subscriptions(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.subscribed[name]
}

// Prompts returns the prompts given to the session so far.
func (s *Sessions) Prompts(name string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.prompts[name]...)
}

func (s *Sessions) channel(name string) chan session.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.events == nil {
		s.events = map[string]chan session.Event{}
	}
	if s.events[name] == nil {
		s.events[name] = make(chan session.Event, 256)
	}
	return s.events[name]
}

// Recording reads a session recorded as lines of {"dir":"in"|"out","msg":{}},
// where "in" is from the agent, and returns its frames in order.
func Recording(path string) ([]session.Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var events []session.Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<24)
	for seq := uint64(1); sc.Scan(); seq++ {
		var line struct {
			Dir string          `json:"dir"`
			Msg json.RawMessage `json:"msg"`
		}
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, seq, err)
		}
		dir := session.ToAgent
		if line.Dir == "in" {
			dir = session.FromAgent
		}
		events = append(events, session.Frame{Seq: seq, Direction: dir, Message: line.Msg})
	}
	return events, sc.Err()
}

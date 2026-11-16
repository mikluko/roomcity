package taskwatch_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"sarak.sh/mikluko/roomcity/internal/session"
	"sarak.sh/mikluko/roomcity/internal/taskwatch"
)

type ended struct {
	mu   sync.Mutex
	got  map[string]string
	done chan string
}

func newEnded() *ended { return &ended{got: map[string]string{}, done: make(chan string, 8)} }

func (e *ended) Fail(_ context.Context, s, reason string) error {
	e.mu.Lock()
	e.got[s] = reason
	e.mu.Unlock()
	e.done <- s
	return nil
}

type registry struct {
	infos  []session.Info
	starts map[string][]session.Start
}

func (r registry) Get(_ context.Context, name string) (session.Info, error) {
	for _, i := range r.infos {
		if i.Name == name {
			return i, nil
		}
	}
	return session.Info{}, session.ErrNotFound
}
func (r registry) List(context.Context) ([]session.Info, error) { return r.infos, nil }
func (r registry) Starts(_ context.Context, name string) ([]session.Start, error) {
	return r.starts[name], nil
}

func frame(d session.Direction, msg string) session.HostFrame {
	return session.HostFrame{Direction: d, Message: []byte(msg)}
}

const prompt = `{"jsonrpc":"2.0","id":%d,"method":"session/prompt","params":{}}`
const failed = `{"jsonrpc":"2.0","id":%d,"error":{"code":-32603,"message":"Authentication required"}}`
const answered = `{"jsonrpc":"2.0","id":%d,"result":{"stopReason":"end_turn"}}`

func turn(id int, response string) []session.HostFrame {
	return []session.HostFrame{
		frame(session.ToAgent, fmt.Sprintf(prompt, id)),
		frame(session.FromAgent, fmt.Sprintf(response, id)),
	}
}

func watch(e *ended, r registry) *taskwatch.Watch {
	return &taskwatch.Watch{
		Sessions:    r,
		Ender:       e,
		Timeout:     func(a string) time.Duration { return map[string]time.Duration{"worker": 30 * time.Minute}[a] },
		FailedTurns: func(a string) int { return map[string]int{"worker": 3}[a] },
	}
}

func TestATaskSessionWhoseModelFailsThreeTurnsInARowIsEndedWithTheLastError(t *testing.T) {
	e := newEnded()
	w := watch(e, registry{infos: []session.Info{{Name: "w-1", Agent: "worker", Kind: session.Task, State: session.Running}}})
	ctx := context.Background()
	w.Observe(ctx, "w-1", turn(1, failed))
	w.Observe(ctx, "w-1", turn(2, failed))
	select {
	case s := <-e.done:
		t.Fatalf("ended %s after two", s)
	case <-time.After(50 * time.Millisecond):
	}
	w.Observe(ctx, "w-1", turn(3, failed))
	require.Equal(t, "w-1", <-e.done)
	require.Equal(t, "ended without a result: its model failed 3 turns in a row, the last with: Authentication required", e.got["w-1"])
}

func TestATurnThatAnswersEndsTheRun(t *testing.T) {
	e := newEnded()
	w := watch(e, registry{infos: []session.Info{{Name: "w-1", Agent: "worker", Kind: session.Task, State: session.Running}}})
	ctx := context.Background()
	w.Observe(ctx, "w-1", turn(1, failed))
	w.Observe(ctx, "w-1", turn(2, failed))
	w.Observe(ctx, "w-1", turn(3, answered))
	w.Observe(ctx, "w-1", turn(4, failed))
	w.Observe(ctx, "w-1", turn(5, failed))
	select {
	case s := <-e.done:
		t.Fatalf("ended %s: the run was broken by an answer", s)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestAnAgentWithNoLimitIsLeftAloneAndSoIsASessionThatIsNoTask(t *testing.T) {
	e := newEnded()
	w := watch(e, registry{infos: []session.Info{
		{Name: "f-1", Agent: "foreman", Kind: session.AlwaysOn, State: session.Running},
		{Name: "w-1", Agent: "worker", Kind: session.AlwaysOn, State: session.Running},
	}})
	for i := 1; i <= 5; i++ {
		w.Observe(context.Background(), "f-1", turn(i, failed))
		w.Observe(context.Background(), "w-1", turn(i, failed))
	}
	select {
	case s := <-e.done:
		t.Fatalf("ended %s", s)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestATaskSessionPastItsAgentsTimeoutIsEndedOnceAndOthersAreNot(t *testing.T) {
	e := newEnded()
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	r := registry{
		infos: []session.Info{
			{Name: "old", Agent: "worker", Kind: session.Task, State: session.Running},
			{Name: "young", Agent: "worker", Kind: session.Task, State: session.Running},
			{Name: "gone", Agent: "worker", Kind: session.Task, State: session.Ended},
			{Name: "foreman", Agent: "foreman", Kind: session.AlwaysOn, State: session.Running},
		},
		starts: map[string][]session.Start{
			"old":     {{N: 1, Began: now.Add(-31 * time.Minute)}},
			"young":   {{N: 1, Began: now.Add(-5 * time.Minute)}},
			"gone":    {{N: 1, Began: now.Add(-3 * time.Hour)}},
			"foreman": {{N: 1, Began: now.Add(-3 * time.Hour)}},
		},
	}
	w := watch(e, r)
	w.Now = func() time.Time { return now }
	w.Check(context.Background())
	w.Check(context.Background())
	require.Equal(t, map[string]string{"old": "ended without a result: timed out after 30m0s"}, e.got)
	require.Len(t, e.done, 1, "once")
}

package chat_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"sarak.sh/mikluko/roomcity/internal/chat"
	"sarak.sh/mikluko/roomcity/internal/chat/chattest"
	"sarak.sh/mikluko/roomcity/internal/session"
)

type sent struct {
	Room    chat.RoomID
	Speaker string
	Msg     chat.Message
}

type retirement struct {
	Room    chat.RoomID
	Speaker string
}

type reaction struct {
	To  chat.MessageID
	Key string
}

// fakeDriver keeps what the service does to the chat system in memory.
type fakeDriver struct {
	mu      sync.Mutex
	specs   []chat.RoomSpec
	sent    []sent
	handler chan func(chat.Incoming)

	retired   []retirement
	opened    int                    // calls of OpenRooms
	pruned    [][]chat.UserID        // the keep of each call of Prune
	open      map[string]chat.RoomID // the open rooms that are in the space, by title
	reactions []reaction
	typings   []bool
	undone    []chat.ReactionID
	// failures is how many calls of EnsureRoom fail before one succeeds.
	failures int
}

func newFakeDriver() *fakeDriver {
	return &fakeDriver{handler: make(chan func(chat.Incoming), 1), open: map[string]chat.RoomID{}}
}

func (d *fakeDriver) EnsureRoom(_ context.Context, spec chat.RoomSpec) (chat.RoomID, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.specs = append(d.specs, spec)
	if len(d.specs) <= d.failures {
		return "", errors.New("the chat system is restarting")
	}
	switch {
	case spec.IsSpace:
		return "!space", nil
	case spec.Open:
		room := chat.RoomID("!" + spec.Title)
		d.open[spec.Title] = room
		return room, nil
	}
	return chat.RoomID("!" + spec.Key), nil
}

func (d *fakeDriver) OpenRooms(context.Context, chat.RoomID) (map[string]chat.RoomID, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.opened++
	out := map[string]chat.RoomID{}
	for title, room := range d.open {
		out[title] = room
	}
	return out, nil
}

// keep puts an open room in the space that the city had before this run.
func (d *fakeDriver) keep(title string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.open[title] = chat.RoomID("!" + title)
}

func (d *fakeDriver) Prune(_ context.Context, keep []chat.UserID) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pruned = append(d.pruned, keep)
	return nil
}

func (d *fakeDriver) Retire(_ context.Context, room chat.RoomID, speaker string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.retired = append(d.retired, retirement{room, speaker})
	for title, r := range d.open {
		if r == room {
			delete(d.open, title)
		}
	}
	return nil
}

func (d *fakeDriver) Link(room chat.RoomID) string { return "link:" + string(room) }

func (d *fakeDriver) Send(_ context.Context, room chat.RoomID, speaker string, msg chat.Message) (chat.MessageID, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sent = append(d.sent, sent{room, speaker, msg})
	return chat.MessageID("$" + msg.Body), nil
}

func (d *fakeDriver) React(_ context.Context, _ chat.RoomID, _ string, to chat.MessageID, key string) (chat.ReactionID, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reactions = append(d.reactions, reaction{to, key})
	return chat.ReactionID(fmt.Sprintf("r%d", len(d.reactions))), nil
}

func (d *fakeDriver) Unreact(_ context.Context, _ chat.RoomID, _ string, r chat.ReactionID) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.undone = append(d.undone, r)
	return nil
}

func (d *fakeDriver) Typing(_ context.Context, _ chat.RoomID, _ string, on bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.typings = append(d.typings, on)
	return nil
}

func (d *fakeDriver) lastTyping() (on, any bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.typings) == 0 {
		return false, false
	}
	return d.typings[len(d.typings)-1], true
}

func (d *fakeDriver) Receive(ctx context.Context, handle func(chat.Incoming)) error {
	d.handler <- handle
	<-ctx.Done()
	return nil
}

func (d *fakeDriver) messages() []sent {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]sent(nil), d.sent...)
}

// roomSpecs returns the specs of the rooms of the always-on agents.
func (d *fakeDriver) roomSpecs() []chat.RoomSpec {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []chat.RoomSpec
	for _, spec := range d.specs {
		if !spec.IsSpace && !spec.Open && spec.Key != "journal" {
			out = append(out, spec)
		}
	}
	return out
}

// allSpecs returns every spec EnsureRoom was called with, in order.
func (d *fakeDriver) allSpecs() []chat.RoomSpec {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]chat.RoomSpec(nil), d.specs...)
}

func (d *fakeDriver) retirements() []retirement {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]retirement(nil), d.retired...)
}

const owner = chat.UserID("@owner:test.local")

func alwaysOn(agent string, state session.State) session.Info {
	return session.Info{Name: "s-" + agent, Agent: agent, Kind: session.AlwaysOn, State: state}
}

// probe is the text awaitRoom sends; promptsOf leaves it out.
const probe = "probe"

// awaitRoom sends owner messages until one reaches the session, which is when
// the room is made and mapped to its session: the room exists a moment before
// the service maps it.
func awaitRoom(t *testing.T, sessions *chattest.Sessions, handle func(chat.Incoming)) {
	t.Helper()
	require.Eventually(t, func() bool {
		handle(chat.Incoming{Room: "!host", Sender: owner, Kind: chat.Text, Body: probe})
		return len(sessions.Prompts("s-host")) > 0
	}, 5*time.Second, 10*time.Millisecond, "the room of host is made and mapped")
}

// promptsOf returns the prompts the host's session got, without the probes.
func promptsOf(sessions *chattest.Sessions) []string {
	var got []string
	for _, p := range sessions.Prompts("s-host") {
		if p != probe {
			got = append(got, p)
		}
	}
	return got
}

// runService starts a service on sessions that already hold the agents.
func runService(t *testing.T, sessions *chattest.Sessions) (*fakeDriver, func(chat.Incoming)) {
	t.Helper()
	return runServiceWith(t, sessions, newFakeDriver())
}

func runServiceWith(t *testing.T, sessions *chattest.Sessions, driver *fakeDriver) (*fakeDriver, func(chat.Incoming)) {
	t.Helper()
	svc := &chat.Service{
		Driver:   driver,
		Sessions: sessions,
		Owners:   []chat.UserID{owner},
		Log:      slog.New(slog.DiscardHandler),

		City:        "testcity",
		Transcripts: "https://t/-/",
		RetryEvery:  10 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- svc.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-done)
	})
	select {
	case handle := <-driver.handler:
		return driver, handle
	case <-time.After(5 * time.Second):
		t.Fatal("the service did not start receiving")
		return nil, nil
	}
}

func TestServiceMakesARoomForEachAlwaysOnAgentAsItsSessionAppears(t *testing.T) {
	sessions := &chattest.Sessions{}
	sessions.Add(alwaysOn("host", session.Running))
	driver, _ := runService(t, sessions)

	// The registry was empty when the server started: guest comes later, and
	// a task's session gets no room here.
	sessions.Add(alwaysOn("guest", session.Starting))
	sessions.Add(session.Info{Name: "s-worker", Agent: "worker", Kind: session.Task, State: session.Running})
	sessions.Add(alwaysOn("guest", session.Running))

	require.Eventually(t, func() bool { return len(driver.roomSpecs()) == 2 }, 5*time.Second, 10*time.Millisecond)
	require.ElementsMatch(t, []chat.RoomSpec{
		{Key: "host", Title: "host", Speaker: "host", Invite: []chat.UserID{owner}, Space: "!space"},
		{Key: "guest", Title: "guest", Speaker: "guest", Invite: []chat.UserID{owner}, Space: "!space"},
	}, driver.roomSpecs())
}

func TestServiceMakesTheRoomOfAnAgentWhoseSessionHasEnded(t *testing.T) {
	sessions := &chattest.Sessions{}
	sessions.Add(alwaysOn("host", session.Ended))
	driver, _ := runService(t, sessions)
	require.Eventually(t, func() bool { return len(driver.roomSpecs()) == 1 }, 5*time.Second, 10*time.Millisecond)
}

func TestServiceTurnsAnOwnersMessageIntoAPrompt(t *testing.T) {
	sessions := &chattest.Sessions{}
	sessions.Add(alwaysOn("host", session.Running))
	_, handle := runService(t, sessions)
	awaitRoom(t, sessions, handle)

	handle(chat.Incoming{Room: "!host", Sender: owner, Kind: chat.Text, Body: "hello"})
	require.Equal(t, []string{"hello"}, promptsOf(sessions))
}

func TestServiceIgnoresWhatIsNotAnOwnersPrompt(t *testing.T) {
	tests := []struct {
		name string
		in   chat.Incoming
	}{
		{"someone without the role", chat.Incoming{Room: "!host", Sender: "@stranger:test.local", Kind: chat.Text, Body: "hi"}},
		{"an edit", chat.Incoming{Room: "!host", Sender: owner, Kind: chat.Text, Body: "* hi", Edit: true}},
		{"a notice", chat.Incoming{Room: "!host", Sender: owner, Kind: chat.Notice, Body: "hi"}},
		{"a command", chat.Incoming{Room: "!host", Sender: owner, Kind: chat.Text, Body: "!stop"}},
		{"a room of no agent", chat.Incoming{Room: "!elsewhere", Sender: owner, Kind: chat.Text, Body: "hi"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sessions := &chattest.Sessions{}
			sessions.Add(alwaysOn("host", session.Running))
			_, handle := runService(t, sessions)
			awaitRoom(t, sessions, handle)

			handle(tt.in)
			handle(chat.Incoming{Room: "!host", Sender: owner, Kind: chat.Text, Body: "marker"})
			require.Equal(t, []string{"marker"}, promptsOf(sessions))
		})
	}
}

func TestServiceSendsARecordedAnswerParagraphByParagraph(t *testing.T) {
	events, err := chattest.Recording("testdata/counter.acp.jsonl")
	require.NoError(t, err)
	want := answerText(t, events)
	sessions := &chattest.Sessions{}
	sessions.Add(alwaysOn("host", session.Running))
	driver, _ := runService(t, sessions)
	require.Eventually(t, func() bool { return len(driver.roomSpecs()) == 1 }, 5*time.Second, 10*time.Millisecond)

	sessions.Emit("s-host", events...)
	// The owner's own prompt is in the stream too, and is not posted again.
	sessions.Emit("s-host", session.Prompt{Seq: 1000, Source: session.FromOwner, Text: "the owner's words", State: session.Stored})

	var got []string
	require.Eventually(t, func() bool {
		got = got[:0]
		for _, m := range driver.messages() {
			require.Equal(t, chat.RoomID("!host"), m.Room)
			require.Equal(t, "host", m.Speaker)
			if m.Msg.Kind == chat.Text && !strings.HasPrefix(m.Msg.Body, "🔐") {
				got = append(got, m.Msg.Body)
			}
		}
		return strings.Join(got, "") == want
	}, 5*time.Second, 10*time.Millisecond)
	require.Greater(t, len(got), 1, "the answer is more than one message")
}

func TestServiceFollowsAnAgentsSessionThatHadEndedBeforeTheServerStarted(t *testing.T) {
	sessions := &chattest.Sessions{}
	sessions.Add(alwaysOn("host", session.Ended))
	driver, _ := runService(t, sessions)
	require.Eventually(t, func() bool { return len(driver.roomSpecs()) == 1 }, 5*time.Second, 10*time.Millisecond)
	require.Eventually(t, func() bool { return sessions.Subscriptions("s-host") == 1 }, 5*time.Second, 10*time.Millisecond,
		"its next start is written to the stream before anyone is told of it: the subscription has to be there already")

	sessions.Add(alwaysOn("host", session.Starting))
	sessions.Emit("s-host", session.Lifecycle{Seq: 7, Kind: session.StartedAfresh})
	require.Eventually(t, func() bool { return len(driver.messagesIn("!journal")) == 1 }, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, 1, sessions.Subscriptions("s-host"), "and it is made once")
}

func TestServiceTellsTheRoomWhenTheSessionBeginsAnew(t *testing.T) {
	sessions := &chattest.Sessions{}
	sessions.Add(alwaysOn("host", session.Running))
	driver, _ := runService(t, sessions)
	require.Eventually(t, func() bool { return len(driver.roomSpecs()) == 1 }, 5*time.Second, 10*time.Millisecond)

	sessions.Emit("s-host", session.Lifecycle{Seq: 7, Kind: session.StartedAfresh})
	require.Eventually(t, func() bool { return len(driver.messagesIn("!host")) == 1 && len(driver.messagesIn("!journal")) == 1 }, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, chat.Notice, driver.messagesIn("!host")[0].Msg.Kind)
	journal := driver.messagesIn("!journal")[0]
	require.Equal(t, "", journal.Speaker, "the city's own user writes the journal")
	require.Contains(t, journal.Msg.Body, "began anew")
	require.Contains(t, journal.Msg.Body, "(link:!host)", "the journal links the room")
}

func (d *fakeDriver) messagesIn(room chat.RoomID) []sent {
	var out []sent
	for _, m := range d.messages() {
		if m.Room == room {
			out = append(out, m)
		}
	}
	return out
}

// answerText joins the text chunks of the agent's message, which is what the
// room should hold once the answer is out.
func answerText(t *testing.T, events []session.Event) string {
	t.Helper()
	var b strings.Builder
	for _, ev := range events {
		f, ok := ev.(session.Frame)
		if !ok || f.Direction != session.FromAgent {
			continue
		}
		var m struct {
			Params struct {
				Update struct {
					Kind    string          `json:"sessionUpdate"`
					Content json.RawMessage `json:"content"`
				} `json:"update"`
			} `json:"params"`
		}
		require.NoError(t, json.Unmarshal(f.Message, &m))
		if m.Params.Update.Kind == "agent_message_chunk" {
			var c struct {
				Text string `json:"text"`
			}
			require.NoError(t, json.Unmarshal(m.Params.Update.Content, &c))
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

func TestServiceTriesAgainUntilTheRoomIsMade(t *testing.T) {
	sessions := &chattest.Sessions{}
	sessions.Add(alwaysOn("host", session.Running))
	driver := newFakeDriver()
	driver.failures = 3
	_, handle := runServiceWith(t, sessions, driver)

	awaitRoom(t, sessions, handle)
	require.Greater(t, len(driver.allSpecs()), 3, "the failed calls were made again")
}

func TestServiceEndsWithAnErrorWhenTheWatchEnds(t *testing.T) {
	sessions := &chattest.Sessions{}
	driver := newFakeDriver()
	svc := &chat.Service{Driver: driver, Sessions: sessions, Log: slog.New(slog.DiscardHandler)}
	done := make(chan error, 1)
	go func() { done <- svc.Run(t.Context()) }()
	<-driver.handler

	sessions.CloseWatch()
	select {
	case err := <-done:
		require.ErrorContains(t, err, "watch")
	case <-time.After(5 * time.Second):
		t.Fatal("Run kept going after the watch ended")
	}
}

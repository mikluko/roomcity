package chat_test

import (
	"bytes"
	"errors"
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

func TestServiceMakesTheSpaceAndTheJournalBeforeAnyRoom(t *testing.T) {
	sessions := &chattest.Sessions{}
	sessions.Add(alwaysOn("host", session.Running))
	driver, handle := runService(t, sessions)
	awaitRoom(t, sessions, handle)

	specs := driver.allSpecs()
	require.GreaterOrEqual(t, len(specs), 3)
	require.Equal(t, chat.RoomSpec{Title: "testcity", IsSpace: true, Invite: []chat.UserID{owner}}, specs[0],
		"the space is called the city's name and every owner is invited to it once")
	require.Equal(t, chat.RoomSpec{Key: "journal", Title: "journal", Invite: []chat.UserID{owner}, Space: "!space"}, specs[1],
		"the journal is in the space and is written by the city's own user")
	require.Equal(t, "host", specs[2].Key)
	require.Equal(t, chat.RoomID("!space"), specs[2].Space)
}

func task(name string, state session.State) session.Info {
	return session.Info{Name: name, Agent: "worker", Kind: session.Task, State: state}
}

func TestServiceRetiresATaskRoomAtTheSessionsEnd(t *testing.T) {
	sessions := &chattest.Sessions{}
	sessions.Add(alwaysOn("host", session.Running))
	driver, handle := runService(t, sessions)
	awaitRoom(t, sessions, handle)

	sessions.Add(task("s-w1", session.Running))
	var spec chat.RoomSpec
	require.Eventually(t, func() bool {
		for _, s := range driver.allSpecs() {
			if s.Open {
				spec = s
			}
		}
		return spec.Open
	}, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, chat.RoomSpec{Title: "worker s-w1", Speaker: "worker", Space: "!space", Open: true}, spec,
		"nobody is invited: the room is open to the space's members, and is named for the agent and the session")
	require.Eventually(t, func() bool { return len(driver.messagesIn("!journal")) == 1 }, 5*time.Second, 10*time.Millisecond)
	started := driver.messagesIn("!journal")[0].Msg.Body
	require.Contains(t, started, "started")
	require.Contains(t, started, "(link:!worker s-w1)", "the journal links the room")

	// The owner steers it with a word.
	require.Eventually(t, func() bool {
		handle(chat.Incoming{Room: "!worker s-w1", Sender: owner, Kind: chat.Text, Body: "mind the tests"})
		return len(sessions.Prompts("s-w1")) > 0
	}, 5*time.Second, 10*time.Millisecond, "an owner's word in a task's room is a prompt")

	// What the agent said last is shown before the room leaves the space, even
	// when the registry says the session has ended while the frames wait.
	sessions.Emit("s-w1", session.Frame{Direction: session.FromAgent, Message: []byte(
		`{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Done."}}}}`)})
	sessions.Add(task("s-w1", session.Ended))
	sessions.Emit("s-w1", session.Lifecycle{Seq: 99, Kind: session.Stopped})

	require.Eventually(t, func() bool { return len(driver.retirements()) == 1 }, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, retirement{Room: "!worker s-w1", Speaker: "worker"}, driver.retirements()[0])
	var last []string
	for _, m := range driver.messagesIn("!worker s-w1") {
		last = append(last, m.Msg.Body)
	}
	require.Equal(t, []string{"Done."}, last)

	require.Eventually(t, func() bool { return len(driver.messagesIn("!journal")) == 2 }, 5*time.Second, 10*time.Millisecond)
	ended := driver.messagesIn("!journal")[1].Msg.Body
	require.Contains(t, ended, "ended")
	require.Contains(t, ended, "(https://t/-/s-w1)", "the journal links the transcript")

	handle(chat.Incoming{Room: "!worker s-w1", Sender: owner, Kind: chat.Text, Body: "too late"})
	require.Equal(t, []string{"mind the tests"}, promptsFor(sessions, "s-w1"), "a room that left the space prompts nothing")
}

func promptsFor(sessions *chattest.Sessions, name string) []string {
	return sessions.Prompts(name)
}

func TestServiceMakesATaskRoomOnceHoweverOftenItsSessionIsAnnounced(t *testing.T) {
	sessions := &chattest.Sessions{}
	sessions.Add(alwaysOn("host", session.Running))
	driver, handle := runService(t, sessions)
	awaitRoom(t, sessions, handle)

	sessions.Add(task("s-w1", session.Starting))
	sessions.Add(task("s-w1", session.Running))
	sessions.Add(task("s-w1", session.Running))
	require.Eventually(t, func() bool { return len(driver.messagesIn("!journal")) >= 1 }, 5*time.Second, 10*time.Millisecond)

	open := 0
	for _, spec := range driver.allSpecs() {
		if spec.Open && strings.HasPrefix(spec.Title, "worker ") {
			open++
		}
	}
	require.Equal(t, 1, open)
}

func TestServiceMakesNoRoomForATaskThatEndedBeforeItWasSeen(t *testing.T) {
	sessions := &chattest.Sessions{}
	sessions.Add(alwaysOn("host", session.Running))
	driver, handle := runService(t, sessions)
	awaitRoom(t, sessions, handle)

	sessions.Add(task("s-old", session.Ended))
	handle(chat.Incoming{Room: "!worker s-old", Sender: owner, Kind: chat.Text, Body: "hello?"})
	require.Empty(t, driver.retirements())
	for _, spec := range driver.allSpecs() {
		require.False(t, spec.Open, "no room is made for a session that is over")
	}
}

func TestServiceRetiresTheRoomOfATaskThatEndedWhileTheServerWasAway(t *testing.T) {
	sessions := &chattest.Sessions{}
	sessions.Add(alwaysOn("host", session.Running))
	sessions.Add(task("s 1/old", session.Ended))
	driver := newFakeDriver()
	driver.keep("worker s 1/old")
	runServiceWith(t, sessions, driver)

	require.Eventually(t, func() bool { return len(driver.retirements()) == 1 }, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, retirement{Room: "!worker s 1/old", Speaker: "worker"}, driver.retirements()[0])
	require.Eventually(t, func() bool { return len(driver.messagesIn("!journal")) == 1 }, 5*time.Second, 10*time.Millisecond)
	require.Contains(t, driver.messagesIn("!journal")[0].Msg.Body, "(https://t/-/s%201%2Fold)", "the session's name is a path segment of the link")
	for _, spec := range driver.allSpecs() {
		require.False(t, spec.Open, "nothing is made for it")
	}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestServiceLogsARetirementFailureOnceAndWritesTheEnd(t *testing.T) {
	sessions := &chattest.Sessions{}
	sessions.Add(alwaysOn("host", session.Running))
	sessions.Add(task("s-old", session.Ended))
	driver := newFakeDriver()
	driver.keep("worker s-old")
	driver.retireErr = errors.New("one member could not be removed")
	var logs lockedBuffer
	runServiceWith(t, sessions, driver, func(svc *chat.Service) {
		svc.Log = slog.New(slog.NewTextHandler(&logs, nil))
	})

	require.Eventually(t, func() bool { return len(driver.messagesIn("!journal")) == 1 }, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, 1, strings.Count(logs.String(), "retiring a task's room"))
	require.Contains(t, logs.String(), "one member could not be removed")
	require.Contains(t, driver.messagesIn("!journal")[0].Msg.Body, "ended")
}

func TestServiceReadsTheRoomsOfTheSpaceOncePerStart(t *testing.T) {
	sessions := &chattest.Sessions{}
	sessions.Add(alwaysOn("host", session.Running))
	driver := newFakeDriver()
	for _, name := range []string{"s-a", "s-b", "s-c"} {
		sessions.Add(task(name, session.Ended))
		driver.keep("worker " + name)
	}
	runServiceWith(t, sessions, driver)

	require.Eventually(t, func() bool { return len(driver.retirements()) == 3 }, 5*time.Second, 10*time.Millisecond)
	driver.mu.Lock()
	defer driver.mu.Unlock()
	require.Equal(t, 1, driver.opened, "one read of the space settles every ended task")
}

func TestServiceWritesAStartedLineOnlyWhenItMadeTheRoom(t *testing.T) {
	sessions := &chattest.Sessions{}
	sessions.Add(alwaysOn("host", session.Running))
	sessions.Add(task("s-w1", session.Running))
	driver := newFakeDriver()
	driver.keep("worker s-w1")
	_, handle := runServiceWith(t, sessions, driver)
	awaitRoom(t, sessions, handle)

	require.Eventually(t, func() bool {
		handle(chat.Incoming{Room: "!worker s-w1", Sender: owner, Kind: chat.Text, Body: "still there?"})
		return len(sessions.Prompts("s-w1")) > 0
	}, 5*time.Second, 10*time.Millisecond, "the room that was found is followed")
	require.Empty(t, driver.messagesIn("!journal"), "a restart does not announce the session again")
}

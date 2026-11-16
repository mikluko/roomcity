package matrix_test

import (
	"context"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"sarak.sh/mikluko/roomcity/internal/chat"
	"sarak.sh/mikluko/roomcity/internal/chat/chattest"
	"sarak.sh/mikluko/roomcity/internal/session"
)

// startService runs a service with the city's name on the sessions.
func startService(t *testing.T, hs string, sessions *chattest.Sessions, owners []chat.UserID) {
	t.Helper()
	driver := newDriver(t, hs)
	serve(t, driver)
	svc := &chat.Service{
		Driver:      driver,
		Sessions:    sessions,
		Owners:      owners,
		Log:         slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})),
		City:        "testcity",
		Transcripts: "https://t/-/",
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- svc.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-done)
	})
}

// roomNamed returns the room of the space whose name is name, or "".
func roomNamed(t *testing.T, reader *appservice.IntentAPI, space id.RoomID, name string) id.RoomID {
	t.Helper()
	for _, child := range spaceChildren(t, reader, space) {
		var content event.RoomNameEventContent
		if reader.StateEvent(t.Context(), child, event.StateRoomName, "", &content) == nil && content.Name == name {
			return child
		}
	}
	return ""
}

func TestServiceBuildsTheCityOfRoomsOnTheHomeserver(t *testing.T) {
	hs := homeserver(t)
	key := uniqueKey()
	alice := userID("alice" + key)
	aliceIntent := newHolders(t, hs).user(t, "alice"+key)

	sessions := &chattest.Sessions{}
	sessions.Add(session.Info{Name: "s-" + key, Agent: key, Kind: session.AlwaysOn, State: session.Running})
	startService(t, hs, sessions, []chat.UserID{alice})

	var space, journal id.RoomID
	require.Eventually(t, func() bool {
		resp, err := aliceIntent.JoinRoom(t.Context(), "#testcity:"+serverName, nil)
		if err != nil {
			return false
		}
		space = resp.RoomID
		resp, err = aliceIntent.JoinRoom(t.Context(), "#testcity-journal:"+serverName, nil)
		if err != nil {
			return false
		}
		journal = resp.RoomID
		return true
	}, 15*time.Second, 200*time.Millisecond, "the owner is invited to the space and to the journal")
	require.Contains(t, spaceChildren(t, aliceIntent, space), journal, "the journal is in the space")
	require.Eventually(t, func() bool { return roomNamed(t, aliceIntent, space, key) != "" },
		15*time.Second, 200*time.Millisecond, "the always-on agent's room is in the space")

	// A session started for a task gets a room in the space.
	task := "t" + key
	sessions.Add(session.Info{Name: "s-" + task, Agent: "worker", Kind: session.Task, State: session.Running})
	var room id.RoomID
	require.Eventually(t, func() bool {
		room = roomNamed(t, aliceIntent, space, "worker s-"+task)
		return room != ""
	}, 15*time.Second, 200*time.Millisecond, "the task's room is in the space")
	_, err := aliceIntent.JoinRoomByID(t.Context(), room)
	require.NoError(t, err, "the owner is in the space, so the room is open to them")

	_, err = aliceIntent.SendText(t.Context(), room, "a word of direction")
	require.NoError(t, err)
	require.Eventually(t, func() bool { return len(sessions.Prompts("s-"+task)) == 1 }, 15*time.Second, 100*time.Millisecond,
		"an owner's word in the task's room reaches the task's session")

	// The session ends: its room leaves the space and the journal says so, with
	// a link to the transcript.
	sessions.Add(session.Info{Name: "s-" + task, Agent: "worker", Kind: session.Task, State: session.Ended})
	sessions.Emit("s-"+task, session.Lifecycle{Seq: 9, Kind: session.Stopped})
	require.Eventually(t, func() bool { return !slices.Contains(spaceChildren(t, aliceIntent, space), room) },
		15*time.Second, 200*time.Millisecond, "the room left the space")
	require.Eventually(t, func() bool {
		var started, ended bool
		for _, body := range bodiesIn(t, aliceIntent, journal, "testcity") {
			started = started || strings.Contains(body, "worker s-"+task) && strings.Contains(body, "started")
			ended = ended || strings.Contains(body, "https://t/-/s-"+task)
		}
		return started && ended
	}, 15*time.Second, 200*time.Millisecond, "the journal links the room, then the transcript")
}

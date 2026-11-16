package matrix_test

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"sarak.sh/mikluko/roomcity/internal/chat"
	"sarak.sh/mikluko/roomcity/internal/chat/chattest"
	"sarak.sh/mikluko/roomcity/internal/chat/matrix"
	"sarak.sh/mikluko/roomcity/internal/server"
	"sarak.sh/mikluko/roomcity/internal/session"
)

// The tests need a Tuwunel with the registrations of scripts/tuwunel-test.sh,
// whose address is in ROOMCITY_TEST_HOMESERVER. The city's transactions come
// to the address in ROOMCITY_TEST_LISTEN, which the script's registration
// names.
const serverName = "test.local"

func homeserver(t *testing.T) string {
	t.Helper()
	hs := os.Getenv("ROOMCITY_TEST_HOMESERVER")
	if hs == "" && os.Getenv("ROOMCITY_TEST_REQUIRE_HOMESERVER") != "" {
		t.Fatal("ROOMCITY_TEST_REQUIRE_HOMESERVER is set, so the homeserver is not optional: ROOMCITY_TEST_HOMESERVER is not")
	}
	if hs == "" {
		t.Skip("ROOMCITY_TEST_HOMESERVER is not set: run scripts/tuwunel-test.sh up")
	}
	return hs
}

func newDriver(t *testing.T, hs string) *matrix.Driver {
	t.Helper()
	d, err := matrix.New(matrix.Config{
		Homeserver: hs,
		ServerName: serverName,
		City:       "testcity",
	}, "testcity-as-token", "testcity-hs-token", slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
	require.NoError(t, err)
	return d
}

// serve mounts the driver's handler on the server and serves it where the
// homeserver sends the city's transactions.
func serve(t *testing.T, d *matrix.Driver) {
	t.Helper()
	listen := os.Getenv("ROOMCITY_TEST_LISTEN")
	if listen == "" {
		listen = "0.0.0.0:29393"
	}
	srv := server.New()
	srv.Handle("/_matrix/app/", d.Handler())
	ln, err := net.Listen("tcp", listen)
	require.NoError(t, err)
	hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = hs.Serve(ln) }()
	t.Cleanup(func() { _ = hs.Close() })
}

// holders returns the application users of the registration testowner, one of
// which the test's service gives the owner's role.
type holders struct {
	as *appservice.AppService
}

func newHolders(t *testing.T, hs string) holders {
	t.Helper()
	reg := appservice.CreateRegistration()
	reg.ID = "testowner"
	reg.SenderLocalpart = "_testowner"
	reg.AppToken = "testowner-as-token"
	reg.ServerToken = "testowner-hs-token"
	as, err := appservice.CreateFull(appservice.CreateOpts{Registration: reg, HomeserverDomain: serverName, HomeserverURL: hs})
	require.NoError(t, err)
	as.HTTPClient.Transport = forwardedFor{http.DefaultTransport}
	return holders{as}
}

type forwardedFor struct{ next http.RoundTripper }

func (f forwardedFor) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("X-Forwarded-For", "127.0.0.1")
	return f.next.RoundTrip(req)
}

// user returns the intent of @_testowner_<name>, made if need be.
func (h holders) user(t *testing.T, name string) *appservice.IntentAPI {
	t.Helper()
	intent := h.as.NewIntentAPI("_testowner_" + name)
	require.NoError(t, intent.EnsureRegistered(t.Context()))
	return intent
}

func userID(name string) chat.UserID {
	return chat.UserID(id.NewUserID("_testowner_"+name, serverName))
}

func uniqueKey() string { return fmt.Sprintf("host%d", time.Now().UnixNano()) }

func TestEnsureRoomFindsTheRoomAgainAfterARestart(t *testing.T) {
	hs := homeserver(t)
	key := uniqueKey()
	alice := userID("alice" + key)
	spec := chat.RoomSpec{Key: key, Title: key, Speaker: "host", Invite: []chat.UserID{alice}}

	first, err := newDriver(t, hs).EnsureRoom(t.Context(), spec)
	require.NoError(t, err)
	require.NotEmpty(t, first)

	// A new driver knows nothing the old one learned.
	again, err := newDriver(t, hs).EnsureRoom(t.Context(), spec)
	require.NoError(t, err)
	require.Equal(t, first, again)

	// A holder added since is invited to the room that exists.
	bob := userID("bob" + key)
	spec.Invite = append(spec.Invite, bob)
	third, err := newDriver(t, hs).EnsureRoom(t.Context(), spec)
	require.NoError(t, err)
	require.Equal(t, first, third)
	h := newHolders(t, hs)
	bobIntent := h.user(t, "bob"+key)
	_, err = bobIntent.JoinRoomByID(t.Context(), id.RoomID(third))
	require.NoError(t, err)

	var levels event.PowerLevelsEventContent
	require.NoError(t, bobIntent.StateEvent(t.Context(), id.RoomID(third), event.StatePowerLevels, "", &levels))
	require.Equal(t, 50, levels.GetUserLevel(id.UserID(bob)), "a holder added later has the holders' power level")
	require.Equal(t, 50, levels.GetUserLevel(id.UserID(alice)))
}

func TestSendAndReactAndType(t *testing.T) {
	hs := homeserver(t)
	key := uniqueKey()
	d := newDriver(t, hs)
	alice := userID("alice" + key)
	room, err := d.EnsureRoom(t.Context(), chat.RoomSpec{Key: key, Title: key, Speaker: "host", Invite: []chat.UserID{alice}})
	require.NoError(t, err)

	first, err := d.Send(t.Context(), room, "host", chat.Message{Kind: chat.Text, Body: "one\n\n**two**"})
	require.NoError(t, err)
	_, err = d.Send(t.Context(), room, "host", chat.Message{Kind: chat.Notice, Body: "a notice", ReplyTo: first})
	require.NoError(t, err)
	_, err = d.Send(t.Context(), room, "host", chat.Message{Kind: chat.Text, Body: "one, edited", Replaces: first})
	require.NoError(t, err)
	_, err = d.Send(t.Context(), room, "host", chat.Message{Kind: chat.Notice, Fold: &chat.Fold{Summary: "output", Text: "a < b", Code: true}})
	require.NoError(t, err)
	thumbs, err := d.React(t.Context(), room, "host", first, "👍")
	require.NoError(t, err)
	require.NoError(t, d.Unreact(t.Context(), room, "host", thumbs))
	require.NoError(t, d.Typing(t.Context(), room, "host", true))
	require.NoError(t, d.Typing(t.Context(), room, "host", false))

	h := newHolders(t, hs)
	reader := h.user(t, "alice"+key)
	_, err = reader.JoinRoomByID(t.Context(), id.RoomID(room))
	require.NoError(t, err)
	bodies := bodiesIn(t, reader, id.RoomID(room), "testcity-host")
	require.Contains(t, bodies, "one\n\n**two**")
	require.Contains(t, bodies, "a notice")
	require.Contains(t, bodies, "* one, edited")
}

// capture records what Service hands to the session behind the seam.
func runService(t *testing.T, hs string, key string, owners []chat.UserID) *chattest.Sessions {
	t.Helper()
	sessions := &chattest.Sessions{}
	sessions.Add(session.Info{Name: "s-" + key, Agent: key, Kind: session.AlwaysOn, State: session.Running})
	driver := newDriver(t, hs)
	serve(t, driver)
	svc := &chat.Service{
		Driver:   driver,
		Sessions: sessions,
		Owners:   owners,
		Log:      slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})),
		City:     "testcity",
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- svc.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-done)
	})
	return sessions
}

func TestAHoldersMessageIsAnOwnersPromptAndTheAnswerComesBack(t *testing.T) {
	hs := homeserver(t)
	key := uniqueKey()
	alice, mallory := userID("alice"+key), userID("mallory"+key)
	h := newHolders(t, hs)

	// Mallory is let into the room by the city, but does not hold the role.
	sessions := runService(t, hs, key, []chat.UserID{alice})
	aliceIntent, malloryIntent := h.user(t, "alice"+key), h.user(t, "mallory"+key)

	var room id.RoomID
	require.Eventually(t, func() bool {
		resp, err := aliceIntent.JoinRoom(t.Context(), "#testcity-"+key+":"+serverName, nil)
		if err != nil {
			return false
		}
		room = resp.RoomID
		return true
	}, 10*time.Second, 200*time.Millisecond, "the room of the agent exists and alice is invited")

	cityBot := cityBotIntent(t, hs)
	require.NoError(t, cityBot.EnsureInvited(t.Context(), room, id.UserID(mallory)))
	_, err := malloryIntent.JoinRoomByID(t.Context(), room)
	require.NoError(t, err)

	_, err = malloryIntent.SendText(t.Context(), room, "from mallory")
	require.NoError(t, err)
	// An image is no prompt: it comes before the text so that a prompt made of
	// it would show first.
	_, err = aliceIntent.SendMessageEvent(t.Context(), room, event.EventMessage, &event.MessageEventContent{MsgType: event.MsgImage, Body: "cat.png"})
	require.NoError(t, err)
	_, err = aliceIntent.SendText(t.Context(), room, "from alice")
	require.NoError(t, err)

	sname := "s-" + key
	require.Eventually(t, func() bool { return len(sessions.Prompts(sname)) > 0 }, 15*time.Second, 100*time.Millisecond,
		"the homeserver delivered the owner's message to the driver")
	require.Equal(t, []string{"from alice"}, sessions.Prompts(sname), "mallory's message and the image changed nothing")

	events, err := chattest.Recording("../testdata/counter.acp.jsonl")
	require.NoError(t, err)
	sessions.Emit(sname, events...)
	require.Eventually(t, func() bool {
		return len(agentMessages(t, aliceIntent, room, key)) >= 2
	}, 15*time.Second, 200*time.Millisecond, "the answer appears in the room")
	bodies := agentMessages(t, aliceIntent, room, key)
	require.Contains(t, bodies, "I'll start with the todo list.")
	require.Contains(t, bodies, "🔧 **ToolSearch**", "a tool call shows as a notice")
	for _, body := range bodies {
		require.NotEmpty(t, body)
	}

	// A permission request that nobody has answered is answered by a reaction
	// of the owner, which reaches the session as the answer. (The recording's
	// own request was answered inside it.)
	sessions.Emit(sname, session.Frame{Direction: session.FromAgent, Message: []byte(
		`{"jsonrpc":"2.0","id":999,"method":"session/request_permission","params":{"toolCall":{"title":"Please approve"},"options":[{"optionId":"allow","name":"Allow","kind":"allow_once"}]}}`)})
	var ask id.EventID
	require.Eventually(t, func() bool {
		ask = findMessage(t, aliceIntent, room, key, "Please approve")
		return ask != ""
	}, 15*time.Second, 200*time.Millisecond)
	_, err = aliceIntent.SendReaction(t.Context(), room, ask, "✅")
	require.NoError(t, err)
	require.Eventually(t, func() bool { return len(sessions.Answers()) == 1 }, 15*time.Second, 100*time.Millisecond,
		"the owner's reaction is the answer")
	require.Equal(t, sname, sessions.Answers()[0].Session)
}

// findMessage returns the id of the agent's message whose body holds text.
func findMessage(t *testing.T, reader *appservice.IntentAPI, room id.RoomID, agent, text string) id.EventID {
	t.Helper()
	msgs, err := reader.Messages(t.Context(), room, "", "", mautrix.DirectionBackward, nil, 100)
	require.NoError(t, err)
	for _, evt := range msgs.Chunk {
		if evt.Type != event.EventMessage || evt.Sender != id.NewUserID("testcity-"+agent, serverName) {
			continue
		}
		require.NoError(t, evt.Content.ParseRaw(evt.Type))
		if strings.Contains(evt.Content.AsMessage().Body, text) {
			return evt.ID
		}
	}
	return ""
}

func cityBotIntent(t *testing.T, hs string) *appservice.IntentAPI {
	t.Helper()
	reg := appservice.CreateRegistration()
	reg.ID = "roomcity-testcity"
	reg.SenderLocalpart = "testcity"
	reg.AppToken = "testcity-as-token"
	reg.ServerToken = "testcity-hs-token"
	as, err := appservice.CreateFull(appservice.CreateOpts{Registration: reg, HomeserverDomain: serverName, HomeserverURL: hs})
	require.NoError(t, err)
	as.HTTPClient.Transport = forwardedFor{http.DefaultTransport}
	return as.BotIntent()
}

// agentMessages returns the bodies of the messages the agent's user sent to
// the room, oldest first.
func agentMessages(t *testing.T, reader *appservice.IntentAPI, room id.RoomID, agent string) []string {
	t.Helper()
	return bodiesIn(t, reader, room, "testcity-"+agent)
}

func bodiesIn(t *testing.T, reader *appservice.IntentAPI, room id.RoomID, localpart string) []string {
	t.Helper()
	msgs, err := reader.Messages(t.Context(), room, "", "", mautrix.DirectionBackward, nil, 100)
	require.NoError(t, err)
	var bodies []string
	for _, evt := range slices.Backward(msgs.Chunk) {
		if evt.Type != event.EventMessage || evt.Sender != id.NewUserID(localpart, serverName) {
			continue
		}
		require.NoError(t, evt.Content.ParseRaw(evt.Type))
		bodies = append(bodies, evt.Content.AsMessage().Body)
	}
	return bodies
}

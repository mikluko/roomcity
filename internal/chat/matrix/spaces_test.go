package matrix_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"sarak.sh/mikluko/roomcity/internal/chat"
)

func TestSpaceHoldsTheRoomsOfTheCityAndOpensATaskRoomToItsMembers(t *testing.T) {
	hs := homeserver(t)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	alice := userID("alice" + suffix)
	d := newDriver(t, hs)
	h := newHolders(t, hs)

	space, err := d.EnsureRoom(t.Context(), chat.RoomSpec{Title: "testcity", IsSpace: true, Invite: []chat.UserID{alice}})
	require.NoError(t, err)
	again, err := newDriver(t, hs).EnsureRoom(t.Context(), chat.RoomSpec{Title: "testcity", IsSpace: true, Invite: []chat.UserID{alice}})
	require.NoError(t, err)
	require.Equal(t, space, again, "the space is found again by the city's name")

	aliceIntent := h.user(t, "alice"+suffix)
	_, err = aliceIntent.JoinRoomByID(t.Context(), id.RoomID(space))
	require.NoError(t, err)
	var created event.CreateEventContent
	require.NoError(t, aliceIntent.StateEvent(t.Context(), id.RoomID(space), event.StateCreate, "", &created))
	require.Equal(t, event.RoomTypeSpace, created.Type)

	title := "worker s-" + suffix
	task := chat.RoomSpec{Title: title, Speaker: "worker", Space: space, Open: true}
	none, err := d.OpenRooms(t.Context(), space)
	require.NoError(t, err)
	require.NotContains(t, none, title, "a room that was never made is not there, and looking makes none")
	room, err := d.EnsureRoom(t.Context(), task)
	require.NoError(t, err)
	found, err := newDriver(t, hs).OpenRooms(t.Context(), space)
	require.NoError(t, err)
	require.Equal(t, room, found[title])
	same, err := newDriver(t, hs).EnsureRoom(t.Context(), task)
	require.NoError(t, err)
	require.Equal(t, room, same, "an open room has no alias: it is found by its name in the space")

	// A member of the space comes in without an invitation; someone outside it
	// does not.
	_, err = aliceIntent.JoinRoomByID(t.Context(), id.RoomID(room))
	require.NoError(t, err, "the space's member joins the task room uninvited")
	_, err = h.user(t, "mallory"+suffix).JoinRoomByID(t.Context(), id.RoomID(room))
	require.ErrorIs(t, err, mautrix.MForbidden, "someone who is not in the space is kept out")

	require.Contains(t, spaceChildren(t, aliceIntent, id.RoomID(space)), id.RoomID(room))

	// The room leaves the space and the agent's user leaves the room.
	require.NoError(t, d.Retire(t.Context(), room, "worker"))
	gone, err := d.OpenRooms(t.Context(), space)
	require.NoError(t, err)
	require.NotContains(t, gone, title, "a room that left the space is no longer listed")
	require.NotContains(t, spaceChildren(t, aliceIntent, id.RoomID(space)), id.RoomID(room))
	members, err := aliceIntent.JoinedMembers(t.Context(), id.RoomID(room))
	require.NoError(t, err)
	require.NotContains(t, members.Joined, id.NewUserID("testcity-worker", serverName))
	require.Contains(t, members.Joined, id.UserID(alice), "the people in the room stay")

	// A room of the same name made after is a new room.
	next, err := d.EnsureRoom(t.Context(), task)
	require.NoError(t, err)
	require.NotEqual(t, room, next)
}

// spaceChildren returns the rooms the space holds now, as a member of it sees.
func spaceChildren(t *testing.T, reader *appservice.IntentAPI, space id.RoomID) []id.RoomID {
	t.Helper()
	state, err := reader.State(t.Context(), space)
	require.NoError(t, err)
	var out []id.RoomID
	for key, evt := range state[event.StateSpaceChild] {
		_ = evt.Content.ParseRaw(event.StateSpaceChild)
		if c := evt.Content.AsSpaceChild(); c != nil && len(c.Via) > 0 {
			out = append(out, id.RoomID(key))
		}
	}
	return out
}

package matrix_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"sarak.sh/mikluko/roomcity/internal/chat"
)

func TestEnsureRoomAfterAHolderJoinedIsNoError(t *testing.T) {
	hs := homeserver(t)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	alice := userID("alice" + suffix)
	h := newHolders(t, hs)
	aliceIntent := h.user(t, "alice"+suffix)

	spaceSpec := chat.RoomSpec{Title: "testcity", IsSpace: true, Invite: []chat.UserID{alice}}
	roomSpec := func(space chat.RoomID) chat.RoomSpec {
		return chat.RoomSpec{Key: "in" + suffix, Title: "in" + suffix, Speaker: "host", Invite: []chat.UserID{alice}, Space: space}
	}
	space, err := newDriver(t, hs).EnsureRoom(t.Context(), spaceSpec)
	require.NoError(t, err)
	room, err := newDriver(t, hs).EnsureRoom(t.Context(), roomSpec(space))
	require.NoError(t, err)
	for _, r := range []chat.RoomID{space, room} {
		_, err = aliceIntent.JoinRoomByID(t.Context(), id.RoomID(r))
		require.NoError(t, err)
	}

	// The server restarts: a driver that remembers nothing asks for the same
	// rooms, and the holder is in them.
	again, err := newDriver(t, hs).EnsureRoom(t.Context(), spaceSpec)
	require.NoError(t, err, "the space, with its holder joined")
	require.Equal(t, space, again)
	again, err = newDriver(t, hs).EnsureRoom(t.Context(), roomSpec(space))
	require.NoError(t, err, "an agent's room, with its holder joined")
	require.Equal(t, room, again)

	// And again, and with the holder still only invited.
	_, err = newDriver(t, hs).EnsureRoom(t.Context(), roomSpec(space))
	require.NoError(t, err)
}

func TestEnsureRoomLeavesABannedHolderAsTheyAre(t *testing.T) {
	hs := homeserver(t)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	alice, bob := userID("alice"+suffix), userID("bob"+suffix)
	h := newHolders(t, hs)
	aliceIntent := h.user(t, "alice"+suffix)
	h.user(t, "bob"+suffix)

	d := newDriver(t, hs)
	spec := chat.RoomSpec{Key: "ban" + suffix, Title: "ban" + suffix, Speaker: "host", Invite: []chat.UserID{alice, bob}}
	room, err := d.EnsureRoom(t.Context(), spec)
	require.NoError(t, err)
	_, err = aliceIntent.JoinRoomByID(t.Context(), id.RoomID(room))
	require.NoError(t, err)
	cityBot := cityBotIntent(t, hs)
	_, err = cityBot.BanUser(t.Context(), id.RoomID(room), &mautrix.ReqBanUser{UserID: id.UserID(bob), Reason: "test"})
	require.NoError(t, err)

	_, err = newDriver(t, hs).EnsureRoom(t.Context(), spec)
	require.NoError(t, err, "a banned holder is no reason to fail, or to make the room again")
	var content event.MemberEventContent
	require.NoError(t, aliceIntent.StateEvent(t.Context(), id.RoomID(room), event.StateMember, string(bob), &content))
	require.Equal(t, event.MembershipBan, content.Membership, "and they are not invited back")
}

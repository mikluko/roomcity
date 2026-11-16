package matrix_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"sarak.sh/mikluko/roomcity/internal/chat"
)

// membership returns what the room's state says of the user's membership, or
// "" if the room has no member event of them.
func membership(t *testing.T, reader *appservice.IntentAPI, room id.RoomID, user id.UserID) event.Membership {
	t.Helper()
	var content event.MemberEventContent
	if err := reader.StateEvent(t.Context(), room, event.StateMember, string(user), &content); err != nil {
		return ""
	}
	return content.Membership
}

func TestPruneRemovesThoseWhoHoldNoRoleFromEveryRoomOfTheCity(t *testing.T) {
	hs := homeserver(t)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	alice, bob, dave := userID("alice"+suffix), userID("bob"+suffix), userID("dave"+suffix)
	d := newDriver(t, hs)
	h := newHolders(t, hs)
	aliceIntent, bobIntent := h.user(t, "alice"+suffix), h.user(t, "bob"+suffix)
	h.user(t, "dave"+suffix)
	both := []chat.UserID{alice, bob}

	space, err := d.EnsureRoom(t.Context(), chat.RoomSpec{Title: "testcity", IsSpace: true, Invite: both})
	require.NoError(t, err)
	agent, err := d.EnsureRoom(t.Context(), chat.RoomSpec{Key: "pr" + suffix, Title: "pr" + suffix, Speaker: "host", Invite: both, Space: space})
	require.NoError(t, err)
	task, err := d.EnsureRoom(t.Context(), chat.RoomSpec{Title: "worker s-" + suffix, Speaker: "worker", Space: space, Open: true})
	require.NoError(t, err)
	for _, user := range []*appservice.IntentAPI{aliceIntent, bobIntent} {
		for _, room := range []chat.RoomID{space, agent, task} {
			_, err := user.JoinRoomByID(t.Context(), id.RoomID(room))
			require.NoError(t, err)
		}
	}
	// Dave was invited to the agent's room and never joined it.
	_, err = d.EnsureRoom(t.Context(), chat.RoomSpec{Key: "pr" + suffix, Title: "pr" + suffix, Speaker: "host", Invite: append(both, dave), Space: space})
	require.NoError(t, err)
	// The task's room has left the space, and its members can still open it.
	require.NoError(t, d.Retire(t.Context(), task, "worker"))
	for _, user := range []*appservice.IntentAPI{aliceIntent, bobIntent} {
		_, err := user.JoinRoomByID(t.Context(), id.RoomID(task))
		require.NoError(t, err)
	}
	require.Equal(t, event.MembershipJoin, membership(t, bobIntent, id.RoomID(task), id.UserID(bob)))

	require.NoError(t, d.Prune(t.Context(), []chat.UserID{alice}))

	for _, room := range []chat.RoomID{space, agent, task} {
		require.Equal(t, event.MembershipJoin, membership(t, aliceIntent, id.RoomID(room), id.UserID(alice)), "the one who holds the role stays")
		require.Equal(t, event.MembershipLeave, membership(t, aliceIntent, id.RoomID(room), id.UserID(bob)), "bob left the role and the room")
	}
	require.Equal(t, event.MembershipLeave, membership(t, aliceIntent, id.RoomID(agent), id.UserID(dave)), "an invitation is taken back")
	require.Equal(t, event.MembershipJoin, membership(t, aliceIntent, id.RoomID(agent), id.NewUserID("testcity-host", serverName)), "the city's own users stay")
}

func TestPruneRefusesToRemoveEveryone(t *testing.T) {
	hs := homeserver(t)
	require.ErrorContains(t, newDriver(t, hs).Prune(t.Context(), nil), "no one is to be kept")
}

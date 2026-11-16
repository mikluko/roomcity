package matrix_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"sarak.sh/mikluko/roomcity/internal/chat"
)

func TestEnsureRoomIsMadeWithTheHoldersThatCanBeInvited(t *testing.T) {
	hs := homeserver(t)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	alice := userID("alice" + suffix)
	// A holder whose id is no id at all: the homeserver answers M_BAD_JSON.
	misspelt := chat.UserID("garbage")
	h := newHolders(t, hs)
	aliceIntent := h.user(t, "alice"+suffix)

	spec := chat.RoomSpec{Key: "one" + suffix, Title: "one" + suffix, Speaker: "host", Invite: []chat.UserID{misspelt, alice}}
	room, err := newDriver(t, hs).EnsureRoom(t.Context(), spec)
	require.NoError(t, err, "one holder's refusal is not the room's")
	require.NotEmpty(t, room)

	_, err = aliceIntent.JoinRoomByID(t.Context(), id.RoomID(room))
	require.NoError(t, err, "the other holder was invited")
	var levels event.PowerLevelsEventContent
	require.NoError(t, aliceIntent.StateEvent(t.Context(), id.RoomID(room), event.StatePowerLevels, "", &levels))
	require.Equal(t, 50, levels.GetUserLevel(id.UserID(alice)))

	again, err := newDriver(t, hs).EnsureRoom(t.Context(), spec)
	require.NoError(t, err, "and again, after a restart")
	require.Equal(t, room, again, "the same room, not another")
}

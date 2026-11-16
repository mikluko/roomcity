package matrix

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func memberEvent(membership event.Membership) *event.Event {
	return &event.Event{Content: event.Content{Parsed: &event.MemberEventContent{Membership: membership}}}
}

func TestRetireMembersContinuesAfterARemovalFails(t *testing.T) {
	city := id.UserID("@testcity:test.local")
	speaker := id.UserID("@testcity-worker:test.local")
	alice := id.UserID("@alice:test.local")
	bob := id.UserID("@bob:test.local")
	carol := id.UserID("@carol:test.local")
	dave := id.UserID("@dave:test.local")
	former := id.UserID("@former:test.local")
	state := mautrix.RoomStateMap{event.StateMember: map[string]*event.Event{
		string(city):    memberEvent(event.MembershipJoin),
		string(speaker): memberEvent(event.MembershipJoin),
		string(alice):   memberEvent(event.MembershipJoin),
		string(bob):     memberEvent(event.MembershipJoin),
		string(carol):   memberEvent(event.MembershipInvite),
		string(dave):    memberEvent(event.MembershipKnock),
		string(former):  memberEvent(event.MembershipLeave),
	}}

	var removed []id.UserID
	left := false
	err := retireMembers("!worker:test.local", city, speaker, state, func(user id.UserID) error {
		removed = append(removed, user)
		if user == alice {
			return errors.New("the homeserver refused")
		}
		return nil
	}, func() error {
		left = true
		return nil
	})

	require.ErrorContains(t, err, "removing "+string(alice)+" from !worker:test.local")
	require.ErrorContains(t, err, "the homeserver refused")
	require.ElementsMatch(t, []id.UserID{alice, bob, carol, dave}, removed)
	require.True(t, left, "the speaker leaves after a member's removal failed")
}

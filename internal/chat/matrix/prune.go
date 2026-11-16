package matrix

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"sarak.sh/mikluko/roomcity/internal/chat"
)

// Prune implements chat.Driver. It looks at every room the city's own user
// made and is in, those that have left the space included, since a member of
// the space can still open them. Anyone in such a room who is neither one of
// the city's own users nor in keep is removed, whether they had joined or only
// been invited. Nothing they said is redacted.
func (d *Driver) Prune(ctx context.Context, keep []chat.UserID) error {
	if len(keep) == 0 {
		return errors.New("matrix: no one is to be kept, so no one is removed")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	bot := d.as.BotIntent()
	rooms, err := bot.JoinedRooms(ctx)
	if err != nil {
		return fmt.Errorf("listing the rooms of the city: %w", err)
	}
	var errs []error
	for _, room := range rooms.JoinedRooms {
		if err := d.pruneRoom(ctx, room, keep); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (d *Driver) pruneRoom(ctx context.Context, room id.RoomID, keep []chat.UserID) error {
	bot := d.as.BotIntent()
	state, err := bot.State(ctx, room)
	if err != nil {
		return fmt.Errorf("reading the state of %s: %w", room, err)
	}
	if create := state[event.StateCreate][""]; create == nil || create.Sender != bot.UserID {
		return nil
	}
	var errs []error
	for key, evt := range state[event.StateMember] {
		user := id.UserID(key)
		_ = evt.Content.ParseRaw(event.StateMember) // an event that is parsed already says so
		member := evt.Content.AsMember()
		if member == nil || !member.Membership.IsInviteOrJoin() && member.Membership != event.MembershipKnock {
			continue
		}
		if d.isOwnUser(user) || slices.Contains(keep, chat.UserID(user)) {
			continue
		}
		if _, err := bot.KickUser(ctx, room, &mautrix.ReqKickUser{UserID: user, Reason: "no longer holds a role in the city"}); err != nil {
			errs = append(errs, fmt.Errorf("removing %s from %s: %w", user, room, err))
			continue
		}
		d.log.Info("removed someone who holds no role from a room", "user", user, "room", room)
	}
	return errors.Join(errs...)
}

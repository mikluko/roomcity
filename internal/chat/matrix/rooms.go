package matrix

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"sarak.sh/mikluko/roomcity/internal/chat"
)

// EnsureRoom implements chat.Driver. A room with a Key is found by its alias,
// #<City>-<Key>:<ServerName>, or #<City>:<ServerName> for the space's, so it
// needs no record of its own; an open room is found by its name among the
// rooms of its space.
func (d *Driver) EnsureRoom(ctx context.Context, spec chat.RoomSpec) (chat.RoomID, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	room, err := d.findOrCreate(ctx, spec)
	if err != nil {
		return "", err
	}
	if err := d.admit(ctx, room, spec); err != nil {
		return "", err
	}
	if spec.Space != "" {
		if err := d.putInSpace(ctx, room, id.RoomID(spec.Space)); err != nil {
			return "", err
		}
	}
	d.rooms[chat.RoomID(room)] = struct{}{}
	return chat.RoomID(room), nil
}

func (d *Driver) findOrCreate(ctx context.Context, spec chat.RoomSpec) (id.RoomID, error) {
	room, err := d.find(ctx, spec)
	if err != nil || room != "" {
		return room, err
	}
	return d.createRoom(ctx, spec)
}

// OpenRooms implements chat.Driver. It reads each room of the space once.
func (d *Driver) OpenRooms(ctx context.Context, space chat.RoomID) (map[string]chat.RoomID, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	children, err := d.children(ctx, id.RoomID(space))
	if err != nil {
		return nil, err
	}
	rooms := map[string]chat.RoomID{}
	for _, child := range children {
		var content event.RoomNameEventContent
		if err := d.as.BotClient().StateEvent(ctx, child, event.StateRoomName, "", &content); err == nil && content.Name != "" {
			rooms[content.Name] = chat.RoomID(child)
		}
	}
	return rooms, nil
}

func (d *Driver) find(ctx context.Context, spec chat.RoomSpec) (id.RoomID, error) {
	if spec.Open {
		return d.findChild(ctx, id.RoomID(spec.Space), spec.Title)
	}
	return d.findRoom(ctx, d.alias(spec.Key))
}

// aliasLocal is the part of the alias between "#" and the colon: the city's
// name for the space, which has the empty key, and <city>-<key> for the rest.
func (d *Driver) aliasLocal(key string) string {
	if key == "" {
		return d.cfg.City
	}
	return d.cfg.City + "-" + key
}

func (d *Driver) alias(key string) id.RoomAlias {
	return id.NewRoomAlias(d.aliasLocal(key), d.cfg.ServerName)
}

func (d *Driver) findRoom(ctx context.Context, alias id.RoomAlias) (id.RoomID, error) {
	resp, err := d.as.BotClient().ResolveAlias(ctx, alias)
	switch {
	case err == nil:
		return resp.RoomID, nil
	case errors.Is(err, mautrix.MNotFound):
		return "", nil
	}
	return "", fmt.Errorf("resolving %s: %w", alias, err)
}

// findChild returns the room of a space that has the name, or "".
func (d *Driver) findChild(ctx context.Context, space id.RoomID, name string) (id.RoomID, error) {
	children, err := d.children(ctx, space)
	if err != nil {
		return "", err
	}
	for _, child := range children {
		var content event.RoomNameEventContent
		err := d.as.BotClient().StateEvent(ctx, child, event.StateRoomName, "", &content)
		if err == nil && content.Name == name {
			return child, nil
		}
	}
	return "", nil
}

// children returns the rooms the space holds now: those whose m.space.child
// event has not been emptied.
func (d *Driver) children(ctx context.Context, space id.RoomID) ([]id.RoomID, error) {
	state, err := d.as.BotClient().State(ctx, space)
	if err != nil {
		return nil, fmt.Errorf("reading the state of %s: %w", space, err)
	}
	var out []id.RoomID
	for key, evt := range state[event.StateSpaceChild] {
		_ = evt.Content.ParseRaw(event.StateSpaceChild) // an event that is parsed already says so
		if c := evt.Content.AsSpaceChild(); c != nil && len(c.Via) > 0 {
			out = append(out, id.RoomID(key))
		}
	}
	return out, nil
}

func (d *Driver) createRoom(ctx context.Context, spec chat.RoomSpec) (id.RoomID, error) {
	// The holders' power levels are not set here: one id the homeserver
	// refuses would refuse the room. inviteHolders sets them, one at a time.
	req := &mautrix.ReqCreateRoom{
		Visibility: "private",
		Preset:     "private_chat",
		Name:       spec.Title,
	}
	if !spec.Open {
		req.RoomAliasName = d.aliasLocal(spec.Key)
	}
	if spec.IsSpace {
		req.CreationContent = map[string]any{"type": event.RoomTypeSpace}
	}
	resp, err := d.as.BotClient().CreateRoom(ctx, req)
	if err != nil {
		return "", fmt.Errorf("creating %q: %w", spec.Title, err)
	}
	if spec.Open {
		if err := d.openToSpace(ctx, resp.RoomID, id.RoomID(spec.Space)); err != nil {
			return "", err
		}
	}
	return resp.RoomID, nil
}

// openToSpace lets the members of a space join a room without an invitation.
func (d *Driver) openToSpace(ctx context.Context, room, space id.RoomID) error {
	rules := &event.JoinRulesEventContent{
		JoinRule: event.JoinRuleRestricted,
		Allow:    []event.JoinRuleAllow{{Type: event.JoinRuleAllowRoomMembership, RoomID: space}},
	}
	if _, err := d.as.BotIntent().SendStateEvent(ctx, room, event.StateJoinRules, "", rules); err != nil {
		return fmt.Errorf("opening %s to the members of %s: %w", room, space, err)
	}
	return nil
}

// admit invites the spec's people, who hold the owner's power level, and lets
// the speaker in.
func (d *Driver) admit(ctx context.Context, room id.RoomID, spec chat.RoomSpec) error {
	if err := d.inviteHolders(ctx, room, spec); err != nil {
		return err
	}
	speaker, err := d.speaker(ctx, spec.Speaker)
	if err != nil {
		return err
	}
	if err := speaker.EnsureJoined(ctx, room); err != nil {
		return fmt.Errorf("letting %s into %q: %w", speaker.UserID, spec.Title, err)
	}
	return nil
}

// inviteHolders invites the spec's people who are not in the room, and gives
// each the owner's power level. Someone who has joined, or been invited
// already, is not invited again, which the homeserver refuses; someone who is
// banned is left as they are, with a word in the log the first time.
// Whoever cannot be invited, or cannot be given the level, for any other
// reason, a misspelt id among them, is logged the same way and passed over: the
// room is made with the others. Only a room whose state cannot be read is an
// error.
func (d *Driver) inviteHolders(ctx context.Context, room id.RoomID, spec chat.RoomSpec) error {
	bot := d.as.BotIntent()
	state, err := bot.State(ctx, room)
	if err != nil {
		return fmt.Errorf("reading the state of %q: %w", spec.Title, err)
	}
	for _, holder := range spec.Invite {
		user := id.UserID(holder)
		switch membership(state, user) {
		case event.MembershipBan:
			d.warnOnce(room, user, "a holder is banned from a room of the city, and is left as they are")
			continue
		case event.MembershipJoin, event.MembershipInvite:
		default:
			if _, err := bot.InviteUser(ctx, room, &mautrix.ReqInviteUser{UserID: user}); err != nil && !alreadyIn(err) {
				d.warnOnce(room, user, "a holder could not be invited to a room of the city, and is passed over: "+err.Error())
				continue
			}
		}
		if _, err := bot.SetPowerLevel(ctx, room, user, ownerPowerLevel); err != nil {
			d.warnOnce(room, user, "a holder could not be given the owner's level in a room of the city, and is passed over: "+err.Error())
		}
	}
	return nil
}

// membership returns what the room's state says of the user's membership, or
// "" if it says nothing.
func membership(state mautrix.RoomStateMap, user id.UserID) event.Membership {
	evt := state[event.StateMember][string(user)]
	if evt == nil {
		return ""
	}
	_ = evt.Content.ParseRaw(event.StateMember) // an event that is parsed already says so
	if m := evt.Content.AsMember(); m != nil {
		return m.Membership
	}
	return ""
}

// alreadyIn reports the homeserver's refusal to invite someone who is in the
// room or banned from it, which a state read a moment before can have missed.
func alreadyIn(err error) bool {
	var httpErr mautrix.HTTPError
	if !errors.As(err, &httpErr) || httpErr.RespError == nil {
		return false
	}
	text := httpErr.RespError.Err
	return strings.Contains(text, "joined or banned") || strings.Contains(text, "already")
}

func (d *Driver) warnOnce(room id.RoomID, user id.UserID, msg string) {
	key := string(room) + " " + string(user)
	if _, seen := d.warned[key]; seen {
		return
	}
	d.warned[key] = struct{}{}
	d.log.Warn(msg, "user", user, "room", room)
}

// putInSpace makes the room a child of the space and the space its parent.
func (d *Driver) putInSpace(ctx context.Context, room, space id.RoomID) error {
	bot := d.as.BotIntent()
	via := []string{d.cfg.ServerName}
	if _, err := bot.SendStateEvent(ctx, space, event.StateSpaceChild, room.String(), &event.SpaceChildEventContent{Via: via}); err != nil {
		return fmt.Errorf("putting %s in %s: %w", room, space, err)
	}
	if _, err := bot.SendStateEvent(ctx, room, event.StateSpaceParent, space.String(), &event.SpaceParentEventContent{Via: via, Canonical: true}); err != nil {
		return fmt.Errorf("naming %s the parent of %s: %w", space, room, err)
	}
	return nil
}

// Retire implements chat.Driver.
func (d *Driver) Retire(ctx context.Context, room chat.RoomID, speaker string) error {
	bot := d.as.BotIntent()
	state, err := bot.State(ctx, id.RoomID(room))
	if err != nil {
		return fmt.Errorf("reading the state of %s: %w", room, err)
	}
	for parent := range state[event.StateSpaceParent] {
		if _, err := bot.SendStateEvent(ctx, id.RoomID(parent), event.StateSpaceChild, string(room), &event.SpaceChildEventContent{}); err != nil {
			return fmt.Errorf("taking %s out of %s: %w", room, parent, err)
		}
		if _, err := bot.SendStateEvent(ctx, id.RoomID(room), event.StateSpaceParent, parent, &event.SpaceParentEventContent{}); err != nil {
			return fmt.Errorf("forgetting the parent %s of %s: %w", parent, room, err)
		}
	}
	intent, err := d.intent(ctx, speaker)
	if err != nil {
		return err
	}
	if intent.UserID == bot.UserID {
		return nil
	}
	if _, err := intent.LeaveRoom(ctx, id.RoomID(room)); err != nil {
		return fmt.Errorf("%s leaving %s: %w", intent.UserID, room, err)
	}
	return nil
}

// Link implements chat.Driver.
func (d *Driver) Link(room chat.RoomID) string {
	return "https://matrix.to/#/" + url.PathEscape(string(room)) + "?via=" + url.QueryEscape(d.cfg.ServerName)
}

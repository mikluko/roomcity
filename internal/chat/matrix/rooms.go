package matrix

import (
	"context"
	"errors"
	"fmt"
	"net/url"

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

// FindRoom implements chat.Driver.
func (d *Driver) FindRoom(ctx context.Context, spec chat.RoomSpec) (chat.RoomID, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	room, err := d.find(ctx, spec)
	return chat.RoomID(room), err
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
	levels := map[id.UserID]int{}
	for _, user := range spec.Invite {
		levels[id.UserID(user)] = ownerPowerLevel
	}
	req := &mautrix.ReqCreateRoom{
		Visibility:         "private",
		Preset:             "private_chat",
		Name:               spec.Title,
		PowerLevelOverride: &event.PowerLevelsEventContent{Users: levels},
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
	bot := d.as.BotIntent()
	for _, user := range spec.Invite {
		if err := bot.EnsureInvited(ctx, room, id.UserID(user)); err != nil {
			return fmt.Errorf("inviting %s to %q: %w", user, spec.Title, err)
		}
		if _, err := bot.SetPowerLevel(ctx, room, id.UserID(user), ownerPowerLevel); err != nil {
			return fmt.Errorf("giving %s power in %q: %w", user, spec.Title, err)
		}
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

// Package matrix is the chat driver for a Matrix homeserver, which the
// server reaches as an application service.
package matrix

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/format"
	"maunium.net/go/mautrix/id"

	"sarak.sh/mikluko/roomcity/internal/chat"
)

// ownerPowerLevel is what the holders of the owner's role get in a room; the
// application service's own user, which makes the room, is above it.
const ownerPowerLevel = 50

// typingTimeout is how long the homeserver keeps a typing notice on.
const typingTimeout = 30 * time.Second

// Driver is a chat.Driver on a Matrix homeserver.
type Driver struct {
	cfg Config
	log *slog.Logger
	as  *appservice.AppService
	ep  *appservice.EventProcessor

	mu       sync.Mutex
	speakers map[string]*appservice.IntentAPI
	rooms    map[chat.RoomID]struct{}
}

var _ chat.Driver = (*Driver)(nil)

// New returns a driver for the configured homeserver, with the tokens that
// Config.Tokens reads. It talks to nobody until a method is called, and
// serves nothing until Handler is mounted.
func New(cfg Config, asToken, hsToken string, log *slog.Logger) (*Driver, error) {
	reg := appservice.CreateRegistration()
	reg.ID = "roomcity-" + cfg.City
	reg.SenderLocalpart = cfg.City
	reg.AppToken = asToken
	reg.ServerToken = hsToken
	as, err := appservice.CreateFull(appservice.CreateOpts{
		Registration:     reg,
		HomeserverDomain: cfg.ServerName,
		HomeserverURL:    cfg.Homeserver,
	})
	if err != nil {
		return nil, fmt.Errorf("creating the application service: %w", err)
	}
	as.HTTPClient.Transport = forwarded{http.DefaultTransport}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Driver{
		cfg:      cfg,
		log:      log,
		as:       as,
		ep:       appservice.NewEventProcessor(as),
		speakers: map[string]*appservice.IntentAPI{},
		rooms:    map[chat.RoomID]struct{}{},
	}, nil
}

// Handler returns what takes the homeserver's transactions, to be mounted at
// "/_matrix/app/". It checks the homeserver's token itself.
func (d *Driver) Handler() http.Handler { return d.as.Router }

// forwarded gives every request an X-Forwarded-For header: Tuwunel answers
// 500 to registration, profile, invite, join and typing requests that have
// none, when its client address comes from that header.
type forwarded struct{ next http.RoundTripper }

func (f forwarded) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("X-Forwarded-For", "127.0.0.1")
	return f.next.RoundTrip(req)
}

// EnsureRoom implements chat.Driver. The room is found by its alias,
// #<City>-<Key>:<ServerName>, so it needs no record of its own.
func (d *Driver) EnsureRoom(ctx context.Context, spec chat.RoomSpec) (chat.RoomID, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	bot := d.as.BotIntent()
	alias := id.NewRoomAlias(d.cfg.City+"-"+spec.Key, d.cfg.ServerName)
	room, err := d.findRoom(ctx, alias)
	if err != nil {
		return "", err
	}
	if room == "" {
		room, err = d.createRoom(ctx, spec, alias)
		if err != nil {
			return "", err
		}
	}
	for _, user := range spec.Invite {
		if err := bot.EnsureInvited(ctx, room, id.UserID(user)); err != nil {
			return "", fmt.Errorf("inviting %s to %s: %w", user, alias, err)
		}
		if _, err := bot.SetPowerLevel(ctx, room, id.UserID(user), ownerPowerLevel); err != nil {
			return "", fmt.Errorf("giving %s power in %s: %w", user, alias, err)
		}
	}
	speaker, err := d.speaker(ctx, spec.Speaker)
	if err != nil {
		return "", err
	}
	if err := speaker.EnsureJoined(ctx, room); err != nil {
		return "", fmt.Errorf("letting %s into %s: %w", speaker.UserID, alias, err)
	}
	d.rooms[chat.RoomID(room)] = struct{}{}
	return chat.RoomID(room), nil
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

func (d *Driver) createRoom(ctx context.Context, spec chat.RoomSpec, alias id.RoomAlias) (id.RoomID, error) {
	levels := map[id.UserID]int{}
	for _, user := range spec.Invite {
		levels[id.UserID(user)] = ownerPowerLevel
	}
	resp, err := d.as.BotClient().CreateRoom(ctx, &mautrix.ReqCreateRoom{
		Visibility:         "private",
		Preset:             "private_chat",
		RoomAliasName:      d.cfg.City + "-" + spec.Key,
		Name:               spec.Title,
		PowerLevelOverride: &event.PowerLevelsEventContent{Users: levels},
	})
	if err != nil {
		return "", fmt.Errorf("creating %s: %w", alias, err)
	}
	return resp.RoomID, nil
}

// speaker returns the intent of the user that speaks for an agent, making the
// user the first time.
func (d *Driver) speaker(ctx context.Context, name string) (*appservice.IntentAPI, error) {
	if intent, ok := d.speakers[name]; ok {
		return intent, nil
	}
	intent := d.as.NewIntentAPI(d.cfg.City + "-" + name)
	if err := intent.EnsureRegistered(ctx); err != nil {
		return nil, fmt.Errorf("registering %s: %w", intent.UserID, err)
	}
	d.speakers[name] = intent
	return intent, nil
}

func (d *Driver) intent(ctx context.Context, name string) (*appservice.IntentAPI, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.speaker(ctx, name)
}

// Send implements chat.Driver.
func (d *Driver) Send(ctx context.Context, room chat.RoomID, speaker string, msg chat.Message) (chat.MessageID, error) {
	intent, err := d.intent(ctx, speaker)
	if err != nil {
		return "", err
	}
	content := format.RenderMarkdown(msg.Body, true, false)
	content.MsgType = event.MsgText
	if msg.Kind == chat.Notice {
		content.MsgType = event.MsgNotice
	}
	if msg.ReplyTo != "" {
		content.RelatesTo = &event.RelatesTo{InReplyTo: &event.InReplyTo{EventID: id.EventID(msg.ReplyTo)}}
	}
	if msg.Replaces != "" {
		content.SetEdit(id.EventID(msg.Replaces))
	}
	resp, err := intent.SendMessageEvent(ctx, id.RoomID(room), event.EventMessage, &content)
	if err != nil {
		return "", fmt.Errorf("sending to %s: %w", room, err)
	}
	return chat.MessageID(resp.EventID), nil
}

// React implements chat.Driver.
func (d *Driver) React(ctx context.Context, room chat.RoomID, speaker string, to chat.MessageID, key string) error {
	intent, err := d.intent(ctx, speaker)
	if err != nil {
		return err
	}
	if _, err := intent.SendReaction(ctx, id.RoomID(room), id.EventID(to), key); err != nil {
		return fmt.Errorf("reacting in %s: %w", room, err)
	}
	return nil
}

// Typing implements chat.Driver.
func (d *Driver) Typing(ctx context.Context, room chat.RoomID, speaker string, on bool) error {
	intent, err := d.intent(ctx, speaker)
	if err != nil {
		return err
	}
	if _, err := intent.UserTyping(ctx, id.RoomID(room), on, typingTimeout); err != nil {
		return fmt.Errorf("typing in %s: %w", room, err)
	}
	return nil
}

// Receive implements chat.Driver. The transactions arrive at Handler, which
// must be mounted; Receive runs what they carry until ctx ends.
func (d *Driver) Receive(ctx context.Context, handle func(chat.Incoming)) error {
	d.ep.On(event.EventMessage, func(_ context.Context, evt *event.Event) {
		if in, ok := d.incoming(evt); ok {
			handle(in)
		}
	})
	d.ep.Start(ctx)
	defer d.ep.Stop()
	<-ctx.Done()
	return nil
}

// incoming turns a room message into a chat.Incoming. It drops what the
// city's own users said and what is in no room of the city.
func (d *Driver) incoming(evt *event.Event) (chat.Incoming, bool) {
	if d.isOwnUser(evt.Sender) {
		return chat.Incoming{}, false
	}
	d.mu.Lock()
	_, known := d.rooms[chat.RoomID(evt.RoomID)]
	d.mu.Unlock()
	if !known {
		return chat.Incoming{}, false
	}
	content := evt.Content.AsMessage()
	if content == nil {
		return chat.Incoming{}, false
	}
	var kind chat.Kind
	switch content.MsgType {
	case event.MsgText:
		kind = chat.Text
	case event.MsgNotice:
		kind = chat.Notice
	default:
		d.log.Info("dropping a message that is not text", "sender", evt.Sender, "msgtype", content.MsgType)
		return chat.Incoming{}, false
	}
	return chat.Incoming{
		Room:    chat.RoomID(evt.RoomID),
		Sender:  chat.UserID(evt.Sender),
		Message: chat.MessageID(evt.ID),
		Kind:    kind,
		Body:    content.Body,
		Edit:    content.RelatesTo != nil && content.RelatesTo.Type == event.RelReplace,
	}, true
}

func (d *Driver) isOwnUser(user id.UserID) bool {
	local, server, err := user.ParseAndValidateRelaxed()
	if err != nil || server != d.cfg.ServerName {
		return false
	}
	return local == d.cfg.City || strings.HasPrefix(local, d.cfg.City+"-")
}

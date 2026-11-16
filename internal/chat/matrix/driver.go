// Package matrix is the chat driver for a Matrix homeserver, which the
// server reaches as an application service.
package matrix

import (
	"context"
	"errors"
	"fmt"
	"html"
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
	warned   map[string]struct{} // what has been logged once already
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
		warned:   map[string]struct{}{},
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

// speaker returns the intent of the user that speaks for an agent, making the
// user the first time.
func (d *Driver) speaker(ctx context.Context, name string) (*appservice.IntentAPI, error) {
	if name == "" {
		return d.as.BotIntent(), nil
	}
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
	content := render(msg)
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
		return "", answered(fmt.Errorf("sending to %s: %w", room, err))
	}
	return chat.MessageID(resp.EventID), nil
}

// React implements chat.Driver.
func (d *Driver) React(ctx context.Context, room chat.RoomID, speaker string, to chat.MessageID, key string) (chat.ReactionID, error) {
	intent, err := d.intent(ctx, speaker)
	if err != nil {
		return "", err
	}
	resp, err := intent.SendReaction(ctx, id.RoomID(room), id.EventID(to), key)
	if err != nil {
		return "", answered(fmt.Errorf("reacting in %s: %w", room, err))
	}
	return chat.ReactionID(resp.EventID), nil
}

// answered marks the error of a call as a refusal when the homeserver answered
// it with an error of the request itself: a status of 4xx, but a rate limit
// (429) or a timeout (408), which is asked again, and a 401, which is the
// homeserver not knowing the application service (it reads its registrations
// at startup) and says nothing of the message. Anything else, the connection
// that failed or a 5xx, is the homeserver out of reach.
func answered(err error) error {
	var httpErr mautrix.HTTPError
	if !errors.As(err, &httpErr) || httpErr.Response == nil {
		return err
	}
	switch code := httpErr.Response.StatusCode; {
	case code == http.StatusTooManyRequests || code == http.StatusRequestTimeout || code == http.StatusUnauthorized:
		return err
	case code >= 400 && code < 500:
		return chat.Refused(err)
	}
	return err
}

// Unreact implements chat.Driver.
func (d *Driver) Unreact(ctx context.Context, room chat.RoomID, speaker string, reaction chat.ReactionID) error {
	intent, err := d.intent(ctx, speaker)
	if err != nil {
		return err
	}
	if _, err := intent.RedactEvent(ctx, id.RoomID(room), id.EventID(reaction)); err != nil {
		return fmt.Errorf("taking a reaction back in %s: %w", room, err)
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
	d.ep.On(event.EventReaction, func(_ context.Context, evt *event.Event) {
		if in, ok := d.reaction(evt); ok {
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

// reaction turns a reaction on a message into a chat.Incoming, with the same
// drops as incoming.
func (d *Driver) reaction(evt *event.Event) (chat.Incoming, bool) {
	if d.isOwnUser(evt.Sender) {
		return chat.Incoming{}, false
	}
	d.mu.Lock()
	_, known := d.rooms[chat.RoomID(evt.RoomID)]
	d.mu.Unlock()
	content := evt.Content.AsReaction()
	if !known || content == nil || content.RelatesTo.Type != event.RelAnnotation {
		return chat.Incoming{}, false
	}
	return chat.Incoming{
		Room:   chat.RoomID(evt.RoomID),
		Sender: chat.UserID(evt.Sender),
		Kind:   chat.Reaction,
		To:     chat.MessageID(content.RelatesTo.EventID),
		Key:    content.RelatesTo.Key,
	}, true
}

func (d *Driver) isOwnUser(user id.UserID) bool {
	local, server, err := user.ParseAndValidateRelaxed()
	if err != nil || server != d.cfg.ServerName {
		return false
	}
	return local == d.cfg.City || strings.HasPrefix(local, d.cfg.City+"-")
}

// render turns a message into Matrix content: its Markdown as HTML, then its
// fold, if any, as a details block.
func render(msg chat.Message) event.MessageEventContent {
	content := format.RenderMarkdown(msg.Body, true, false)
	if strings.Contains(content.FormattedBody, "<table>") {
		// The plain text mautrix makes of a table is its cells run together.
		content.Body = msg.Body
	}
	f := msg.Fold
	if f == nil {
		return content
	}
	if content.FormattedBody == "" {
		content.Format = event.FormatHTML
		if msg.Body != "" {
			content.FormattedBody = "<p>" + html.EscapeString(msg.Body) + "</p>"
		}
	}
	// A message that is only a fold has no words of its own: its summary is
	// the plain text's first line, and not an empty one.
	head := content.Body
	if head == "" {
		head = f.Summary
	}
	content.Body = head + "\n" + f.Text
	text := html.EscapeString(f.Text)
	if f.Code {
		text = "<pre><code>" + text + "</code></pre>"
	} else {
		text = "<blockquote>" + text + "</blockquote>"
	}
	content.FormattedBody += "<details><summary>" + html.EscapeString(f.Summary) + "</summary>" + text + "</details>"
	return content
}

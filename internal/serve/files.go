package serve

import (
	"context"
	"errors"

	"sarak.sh/mikluko/roomcity/internal/chat"
	"sarak.sh/mikluko/roomcity/internal/server"
)

// attacher is the chat service as the adapter uses it.
type attacher interface {
	Attach(ctx context.Context, session string, f chat.File) (chat.MessageID, error)
}

// roomFiles is server.Files over the chat service: a file a session's agent
// sends is attached to the session's room.
type roomFiles struct{ chat attacher }

func (r roomFiles) SendFile(ctx context.Context, session string, f server.File) (string, error) {
	id, err := r.chat.Attach(ctx, session, chat.File{Name: f.Name, MediaType: f.MediaType, Caption: f.Caption, Data: f.Data})
	if err != nil {
		return "", asServerError(err)
	}
	return string(id), nil
}

// refusal is a chat error that is also the server's error of the same
// meaning, with the chat's text, which names the bound for the agent to read.
type refusal struct{ chat, server error }

func (r refusal) Error() string   { return r.chat.Error() }
func (r refusal) Unwrap() []error { return []error{r.chat, r.server} }

// asServerError makes of a refusal of the chat side the error that Send maps
// to its code: the two sets of errors are different values.
func asServerError(err error) error {
	for _, m := range []struct{ chat, server error }{
		{chat.ErrFileTooLarge, server.ErrFileTooLarge},
		{chat.ErrFileKind, server.ErrFileKind},
		{chat.ErrNoRoom, server.ErrNoRoom},
		{chat.ErrChatDown, server.ErrChatDown},
	} {
		if errors.Is(err, m.chat) {
			return refusal{err, m.server}
		}
	}
	return err
}

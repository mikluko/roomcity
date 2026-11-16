package serve

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"sarak.sh/mikluko/roomcity/internal/chat"
	"sarak.sh/mikluko/roomcity/internal/chat/matrix"
	"sarak.sh/mikluko/roomcity/internal/config"
	"sarak.sh/mikluko/roomcity/internal/session"
)

// appServicePath is where the homeserver's transactions arrive, on the
// network listener.
const appServicePath = "/_matrix/app/"

// chatSide is the chat service of a city and the handler of its homeserver's
// transactions, made from the configuration's [chat] and [matrix] tables.
type chatSide struct {
	Service *chat.Service
	Handler http.Handler
}

// newChatSide makes the chat side, or none when the configuration has no chat:
// neither table. A [chat] without its [matrix], or a [matrix] whose tokens
// cannot be read, is an error: the server would otherwise start without the
// city's rooms and say nothing.
func newChatSide(cfg config.Config, reg *session.Registry, log *slog.Logger) (*chatSide, error) {
	if cfg.Matrix == nil {
		if len(cfg.Chat.Holders) > 0 || cfg.Chat.Name != "" || cfg.Chat.Transcripts != "" {
			return nil, errors.New("[chat] is configured and [matrix] is not: there is no chat system to give the city's rooms to")
		}
		return nil, nil
	}
	asToken, hsToken, err := cfg.Matrix.Tokens()
	if err != nil {
		return nil, err
	}
	driver, err := matrix.New(*cfg.Matrix, asToken, hsToken, log)
	if err != nil {
		return nil, fmt.Errorf("making the Matrix driver: %w", err)
	}
	name := cfg.Chat.Name
	if name == "" {
		name = cfg.Matrix.City
	}
	return &chatSide{
		Handler: driver.Handler(),
		Service: &chat.Service{
			Driver:      driver,
			Sessions:    reg,
			Owners:      cfg.Chat.Owners(),
			Log:         log,
			City:        name,
			Transcripts: cfg.Chat.Transcripts,
		},
	}, nil
}

// city is the name the transcripts' pages carry: the chat's, or the Matrix
// city's when the file names none; empty with no chat.
func (c *chatSide) city(cfg config.Config) string {
	if c == nil {
		return cfg.Chat.Name
	}
	return c.Service.City
}

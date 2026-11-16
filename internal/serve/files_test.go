package serve

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"sarak.sh/mikluko/roomcity/internal/chat"
	"sarak.sh/mikluko/roomcity/internal/server"
)

type attached struct {
	session string
	file    chat.File
	err     error
}

func (a *attached) Attach(_ context.Context, session string, f chat.File) (chat.MessageID, error) {
	a.session, a.file = session, f
	if a.err != nil {
		return "", a.err
	}
	return "$ev", nil
}

func TestTheFileReachesTheChatSideAndItsEventComesBack(t *testing.T) {
	a := &attached{}
	id, err := roomFiles{a}.SendFile(context.Background(), "s", server.File{Name: "a.png", MediaType: "image/png", Caption: "c", Data: []byte("x")})
	require.NoError(t, err)
	require.Equal(t, "$ev", id)
	require.Equal(t, "s", a.session)
	require.Equal(t, chat.File{Name: "a.png", MediaType: "image/png", Caption: "c", Data: []byte("x")}, a.file)
}

func TestEachRefusalOfTheChatSideIsTheServersOfTheSameMeaningWithTheChatsText(t *testing.T) {
	for chatErr, serverErr := range map[error]error{
		chat.ErrFileTooLarge: server.ErrFileTooLarge,
		chat.ErrFileKind:     server.ErrFileKind,
		chat.ErrNoRoom:       server.ErrNoRoom,
		chat.ErrChatDown:     server.ErrChatDown,
	} {
		wrapped := fmt.Errorf("%w: the text the agent reads", chatErr)
		_, err := roomFiles{&attached{err: wrapped}}.SendFile(context.Background(), "s", server.File{})
		require.ErrorIs(t, err, serverErr)
		require.ErrorIs(t, err, chatErr)
		require.Equal(t, wrapped.Error(), err.Error())
	}
}

func TestAnErrorThatIsNoRefusalPassesAsItIs(t *testing.T) {
	boom := errors.New("boom")
	_, err := roomFiles{&attached{err: boom}}.SendFile(context.Background(), "s", server.File{})
	require.Same(t, boom, err)
}

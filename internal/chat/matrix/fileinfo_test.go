package matrix

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"

	"sarak.sh/mikluko/roomcity/internal/chat"
)

func TestFileContentOfAPictureThatCannotBeReadIsAFile(t *testing.T) {
	c := fileContent(chat.File{Name: "broken.png", MediaType: "image/png", Data: []byte("not a png")}, "mxc://x/y")
	require.Equal(t, event.MsgFile, c.MsgType)
	require.Equal(t, "broken.png", c.Body)
	require.EqualValues(t, 9, c.Info.Size)
}

func TestUploadFailedTellsAFileTooLargeFromWhatIsRefusedAndWhatIsOutOfReach(t *testing.T) {
	status := func(code int) error {
		return fmt.Errorf("uploading: %w", mautrix.HTTPError{Response: &http.Response{StatusCode: code}})
	}
	require.ErrorIs(t, uploadFailed(status(413)), chat.ErrFileTooLarge)
	require.True(t, chat.IsRefused(uploadFailed(status(403))))
	err := uploadFailed(status(502))
	require.False(t, chat.IsRefused(err))
	require.False(t, errors.Is(err, chat.ErrFileTooLarge))
}

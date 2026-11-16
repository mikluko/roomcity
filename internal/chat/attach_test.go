package chat_test

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/pelletier/go-toml/v2"
	"github.com/stretchr/testify/require"

	"sarak.sh/mikluko/roomcity/internal/chat"
	"sarak.sh/mikluko/roomcity/internal/chat/chattest"
	"sarak.sh/mikluko/roomcity/internal/session"
)

// attachService runs a service with the room of host made, and returns it
// with what it needs: the service is reached through Attach, which the test
// calls as the server does.
func attachService(t *testing.T, driver *fakeDriver, options ...func(*chat.Service)) (*chat.Service, *chattest.Sessions) {
	t.Helper()
	sessions := &chattest.Sessions{}
	sessions.Add(alwaysOn("host", session.Running))
	var svc *chat.Service
	options = append(options, func(s *chat.Service) { svc = s })
	_, handle := runServiceWith(t, sessions, driver, options...)
	awaitRoom(t, sessions, handle)
	return svc, sessions
}

func TestAttachSendsTheFileToTheRoomAsTheAgent(t *testing.T) {
	driver := newFakeDriver()
	svc, _ := attachService(t, driver)

	f := chat.File{Name: "chart.png", MediaType: "image/png", Caption: "GDP", Data: append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{1}, 100)...)}
	id, err := svc.Attach(t.Context(), "s-host", f)
	require.NoError(t, err)
	require.Equal(t, chat.MessageID("$file chart.png"), id)
	require.Equal(t, []sentFile{{Room: "!host", Speaker: "host", File: f}}, driver.files)
}

func TestAttachRefusesWhatTheRoomDoesNotTakeAndSaysWhy(t *testing.T) {
	driver := newFakeDriver()
	svc, _ := attachService(t, driver, func(s *chat.Service) { s.MaxFile = 1 << 10 })

	_, err := svc.Attach(t.Context(), "s-host", chat.File{Name: "big.png", MediaType: "image/png", Data: make([]byte, 3<<10)})
	require.ErrorIs(t, err, chat.ErrFileTooLarge)
	require.Contains(t, err.Error(), "big.png is 3.0 KiB and the room takes 1.0 KiB")

	_, err = svc.Attach(t.Context(), "s-host", chat.File{Name: "clip.mp4", MediaType: "video/mp4", Data: []byte("x")})
	require.ErrorIs(t, err, chat.ErrFileKind)
	require.Contains(t, err.Error(), "video/mp4")
	require.Contains(t, err.Error(), "image/*", "it names what the room takes")

	_, err = svc.Attach(t.Context(), "s-nobody", chat.File{Name: "a.txt", MediaType: "text/plain", Data: []byte("x")})
	require.ErrorIs(t, err, chat.ErrNoRoom)

	require.Empty(t, driver.files, "none of them was sent")
}

func TestAttachTakesTheKindsTheConfigurationNames(t *testing.T) {
	driver := newFakeDriver()
	svc, _ := attachService(t, driver, func(s *chat.Service) { s.FileKinds = []string{"video/mp4"} })

	_, err := svc.Attach(t.Context(), "s-host", chat.File{Name: "clip.mp4", MediaType: "video/mp4", Data: []byte("\x00\x00\x00\x0cftypmp42")})
	require.NoError(t, err)
	_, err = svc.Attach(t.Context(), "s-host", chat.File{Name: "a.png", MediaType: "image/png", Data: []byte("x")})
	require.ErrorIs(t, err, chat.ErrFileKind)
	_, err = svc.Attach(t.Context(), "s-host", chat.File{Name: "a", MediaType: "not a type;;", Data: []byte("x")})
	require.ErrorIs(t, err, chat.ErrFileKind, "a media type that does not parse is of no kind")
}

func TestAttachSaysWhetherTheChatSystemIsOutOfReachOrRefusedTheFile(t *testing.T) {
	driver := newFakeDriver()
	svc, _ := attachService(t, driver)
	file := chat.File{Name: "a.txt", MediaType: "text/plain", Data: []byte("x")}

	driver.fileErr = errors.New("dial tcp: connection refused")
	_, err := svc.Attach(t.Context(), "s-host", file)
	require.ErrorIs(t, err, chat.ErrChatDown)

	driver.fileErr = chat.Refused(errors.New("M_FORBIDDEN"))
	_, err = svc.Attach(t.Context(), "s-host", file)
	require.Error(t, err)
	require.False(t, errors.Is(err, chat.ErrChatDown), "a homeserver that answered is not out of reach")
	require.Contains(t, err.Error(), "refused the file")

	driver.fileErr = fmt.Errorf("the homeserver takes less: %w", chat.ErrFileTooLarge)
	_, err = svc.Attach(t.Context(), "s-host", file)
	require.ErrorIs(t, err, chat.ErrFileTooLarge, "what the driver says of the size is what the agent is told")
}

func TestConfigTakesTheBoundsOfWhatAgentsSend(t *testing.T) {
	var file struct {
		Chat chat.Config `toml:"chat"`
	}
	require.NoError(t, toml.Unmarshal([]byte("[chat]\nmax_file = \"512KiB\"\nfile_kinds = [\"image/*\", \"text/csv\"]\n"), &file))
	require.NoError(t, file.Chat.Validate())
	require.EqualValues(t, 512<<10, file.Chat.MaxFile)
	require.Equal(t, []string{"image/*", "text/csv"}, file.Chat.FileKinds)

	require.NoError(t, toml.Unmarshal([]byte("[chat]\nmax_file = 2048\n"), &file))
	require.EqualValues(t, 2048, file.Chat.MaxFile, "a plain number is bytes")

	require.Error(t, toml.Unmarshal([]byte("[chat]\nmax_file = \"lots\"\n"), &file))
	require.NoError(t, toml.Unmarshal([]byte("[chat]\nfile_kinds = [\"image\"]\n"), &file))
	require.ErrorContains(t, file.Chat.Validate(), "file_kinds")
}

func TestAttachLooksAtTheBytesAndNotOnlyAtTheKindTheCallerNames(t *testing.T) {
	driver := newFakeDriver()
	svc, _ := attachService(t, driver)

	zip := []byte("PK\x03\x04\x14\x00\x00\x00\x08\x00 the rest of an archive")
	_, err := svc.Attach(t.Context(), "s-host", chat.File{Name: "notes.txt", MediaType: "text/plain", Data: zip})
	require.ErrorIs(t, err, chat.ErrFileKind, "a zip called text")
	require.Contains(t, err.Error(), "application/zip")

	_, err = svc.Attach(t.Context(), "s-host", chat.File{Name: "chart.png", MediaType: "image/png", Data: []byte("<!DOCTYPE html><html><body>hi</body></html>")})
	require.ErrorIs(t, err, chat.ErrFileKind, "an HTML page called a picture")
	require.Contains(t, err.Error(), "text/html")

	_, err = svc.Attach(t.Context(), "s-host", chat.File{Name: "doc.pdf", MediaType: "application/pdf", Data: []byte("just words")})
	require.ErrorIs(t, err, chat.ErrFileKind, "words called a PDF")

	require.Empty(t, driver.files)

	pdf := []byte("%PDF-1.4 the rest of a document")
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR the rest of a picture")
	for _, f := range []chat.File{
		{Name: "doc.pdf", MediaType: "application/pdf", Data: pdf},
		{Name: "chart.png", MediaType: "image/png", Data: png},
		{Name: "data.csv", MediaType: "text/csv", Data: []byte("a,b\n1,2\n")},
		{Name: "data.json", MediaType: "application/json", Data: []byte(`{"a": 1}`)},
		{Name: "drawing.svg", MediaType: "image/svg+xml", Data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`)},
	} {
		_, err := svc.Attach(t.Context(), "s-host", f)
		require.NoError(t, err, f.Name)
	}
	require.Len(t, driver.files, 5)

	_, err = svc.Attach(t.Context(), "s-host", chat.File{Name: "notes.txt", MediaType: "text/plain; charset=utf-8", Data: []byte("words")})
	require.NoError(t, err)
	require.Equal(t, "text/plain", driver.files[5].File.MediaType, "it is sent under the kind that was checked, without parameters")
}

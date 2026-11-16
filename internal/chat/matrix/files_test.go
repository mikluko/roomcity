package matrix_test

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"sarak.sh/mikluko/roomcity/internal/chat"
)

func TestSendFileShowsAPictureAsAnImageAndAnythingElseAsAFile(t *testing.T) {
	hs := homeserver(t)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	alice := userID("alice" + suffix)
	h := newHolders(t, hs)
	aliceIntent := h.user(t, "alice"+suffix)
	d := newDriver(t, hs)

	room, err := d.EnsureRoom(t.Context(), chat.RoomSpec{Key: "files" + suffix, Title: "files" + suffix, Speaker: "host", Invite: []chat.UserID{alice}})
	require.NoError(t, err)
	_, err = aliceIntent.JoinRoomByID(t.Context(), id.RoomID(room))
	require.NoError(t, err)

	var pic bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 7, 3))
	img.Set(1, 1, color.RGBA{R: 255, A: 255})
	require.NoError(t, png.Encode(&pic, img))

	_, err = d.SendFile(t.Context(), room, "host", chat.File{Name: "chart.png", MediaType: "image/png", Caption: "GDP", Data: pic.Bytes()})
	require.NoError(t, err)
	_, err = d.SendFile(t.Context(), room, "host", chat.File{Name: "notes.txt", MediaType: "text/plain", Data: []byte("hello")})
	require.NoError(t, err)
	_, err = d.SendFile(t.Context(), room, "host", chat.File{Name: "drawing.svg", MediaType: "image/svg+xml", Data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)})
	require.NoError(t, err)

	msgs, err := aliceIntent.Messages(t.Context(), id.RoomID(room), "", "", mautrix.DirectionForward, nil, 50)
	require.NoError(t, err)
	byName := map[string]*event.MessageEventContent{}
	for _, evt := range msgs.Chunk {
		if evt.Type != event.EventMessage {
			continue
		}
		require.NoError(t, evt.Content.ParseRaw(evt.Type))
		m := evt.Content.AsMessage()
		if m.MsgType == event.MsgImage || m.MsgType == event.MsgFile {
			name := m.FileName
			if name == "" {
				name = m.Body
			}
			byName[name] = m
		}
	}

	chart := byName["chart.png"]
	require.NotNil(t, chart)
	require.Equal(t, event.MsgImage, chart.MsgType)
	require.Equal(t, "GDP", chart.Body, "the caption is the body, the name the filename")
	require.Equal(t, 7, chart.Info.Width)
	require.Equal(t, 3, chart.Info.Height)
	require.Equal(t, "image/png", chart.Info.MimeType)
	uri, err := chart.URL.Parse()
	require.NoError(t, err)
	got, err := aliceIntent.DownloadBytes(t.Context(), uri)
	require.NoError(t, err)
	require.Equal(t, pic.Bytes(), got, "the file is in the media repository")

	notes := byName["notes.txt"]
	require.NotNil(t, notes)
	require.Equal(t, event.MsgFile, notes.MsgType)
	require.Equal(t, "notes.txt", notes.Body, "with no caption the name is the body")

	svg := byName["drawing.svg"]
	require.NotNil(t, svg)
	require.Equal(t, event.MsgFile, svg.MsgType, "an SVG is a file until a client is seen to draw one")
}

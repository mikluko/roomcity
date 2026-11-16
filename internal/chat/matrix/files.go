package matrix

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/gif" // the formats of an image whose size is read
	_ "image/jpeg"
	_ "image/png"
	"mime"
	"net/http"
	"strings"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"sarak.sh/mikluko/roomcity/internal/chat"
)

// SendFile implements chat.Driver. The file is uploaded to the media
// repository as the speaker and sent as an m.image when it is a picture whose
// size can be read (a PNG, a JPEG, a GIF), and as an m.file otherwise: an SVG
// is a file, until a client is seen to draw one. The caption is the body and
// the name the filename; with no caption the name is the body.
func (d *Driver) SendFile(ctx context.Context, room chat.RoomID, speaker string, f chat.File) (chat.MessageID, error) {
	intent, err := d.intent(ctx, speaker)
	if err != nil {
		return "", err
	}
	up, err := intent.UploadMedia(ctx, mautrix.ReqUploadMedia{ContentBytes: f.Data, ContentType: f.MediaType, FileName: f.Name})
	if err != nil {
		return "", uploadFailed(fmt.Errorf("uploading %s: %w", f.Name, err))
	}
	content := fileContent(f, up.ContentURI.CUString())
	resp, err := intent.SendMessageEvent(ctx, id.RoomID(room), event.EventMessage, &content)
	if err != nil {
		return "", answered(fmt.Errorf("sending %s to %s: %w", f.Name, room, err))
	}
	return chat.MessageID(resp.EventID), nil
}

// fileContent is the event content that shows the uploaded file.
func fileContent(f chat.File, uri id.ContentURIString) event.MessageEventContent {
	info := &event.FileInfo{MimeType: f.MediaType, Size: len(f.Data)}
	content := event.MessageEventContent{MsgType: event.MsgFile, Body: f.Name, URL: uri, Info: info}
	if f.Caption != "" {
		content.Body, content.FileName = f.Caption, f.Name
	}
	if media, _, err := mime.ParseMediaType(f.MediaType); err == nil && strings.HasPrefix(media, "image/") {
		if cfg, _, err := image.DecodeConfig(bytes.NewReader(f.Data)); err == nil {
			content.MsgType = event.MsgImage
			info.Width, info.Height = cfg.Width, cfg.Height
		}
	}
	return content
}

// uploadFailed marks the error of an upload: a homeserver that finds the file
// too large says so with a 413, which the agent is told; any other answer of
// 4xx is a refusal and the rest the homeserver being out of reach.
func uploadFailed(err error) error {
	var httpErr mautrix.HTTPError
	if errors.As(err, &httpErr) && httpErr.Response != nil && httpErr.Response.StatusCode == http.StatusRequestEntityTooLarge {
		return fmt.Errorf("%w: the homeserver takes less: %v", chat.ErrFileTooLarge, err)
	}
	return answered(err)
}

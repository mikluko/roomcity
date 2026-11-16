package chat

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
)

// File is a file sent to a room: what an agent attaches.
type File struct {
	// Name is the file's name, without a directory.
	Name string
	// MediaType is the file's media type, such as image/png.
	MediaType string
	// Caption is the words that go with the file; empty for none.
	Caption string
	Data    []byte
}

// What a refused file is refused for. Attach wraps one of them in an error
// whose text says why, in words the agent can read; errors.Is finds which.
var (
	// ErrFileTooLarge is a file over what the room takes.
	ErrFileTooLarge = errors.New("the file is larger than the room takes")
	// ErrFileKind is a file of a kind the room does not take.
	ErrFileKind = errors.New("the room does not take a file of that kind")
	// ErrNoRoom is a session that has no room to send to.
	ErrNoRoom = errors.New("the session has no room")
	// ErrChatDown is a chat system that cannot be reached.
	ErrChatDown = errors.New("the chat system cannot be reached")
)

// Size is a number of bytes written as a number, or with a unit: "512KiB",
// "10MiB".
type Size int64

var sizeUnits = map[string]int64{"": 1, "B": 1, "KIB": 1 << 10, "MIB": 1 << 20, "GIB": 1 << 30}

// UnmarshalText reads a size.
func (s *Size) UnmarshalText(b []byte) error {
	text := strings.ToUpper(strings.TrimSpace(string(b)))
	digits := strings.TrimRight(text, "BIKMG ")
	n, err := strconv.ParseInt(strings.TrimSpace(digits), 10, 64)
	if err != nil || n < 0 {
		return fmt.Errorf("%q is not a size, such as 10MiB", b)
	}
	unit, ok := sizeUnits[strings.TrimSpace(text[len(digits):])]
	if !ok {
		return fmt.Errorf("%q is not a size, such as 10MiB", b)
	}
	*s = Size(n * unit)
	return nil
}

// Default bounds of what the room takes from an agent.
const DefaultMaxFile = Size(10 << 20)

// DefaultFileKinds are the media types a room takes when the configuration
// names none.
var DefaultFileKinds = []string{"image/*", "text/*", "application/pdf", "application/json"}

// allowed says whether the media type is one of the kinds: each is a media
// type, or a type and "/*" for all of it.
func allowed(kinds []string, mediaType string) bool {
	media, _, err := mime.ParseMediaType(mediaType)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(kinds, func(kind string) bool {
		ok, _ := path.Match(kind, media)
		return ok
	})
}

// Attach sends the file to the room of the session, as the session's agent,
// after the bounds of [chat]: a file over max_file or of a kind that file_kinds
// does not list is refused, with an error that names the bound.
func (s *Service) Attach(ctx context.Context, name string, f File) (MessageID, error) {
	t := s.targetOf(name)
	if t == nil {
		return "", fmt.Errorf("%w: %s has none, or its room is not made yet", ErrNoRoom, name)
	}
	if max := s.maxFile(); int64(len(f.Data)) > max {
		return "", fmt.Errorf("%w: %s is %s and the room takes %s", ErrFileTooLarge, f.Name, sizeText(int64(len(f.Data))), sizeText(max))
	}
	if !allowed(s.fileKinds(), f.MediaType) {
		return "", fmt.Errorf("%w: %s is %q, and the room takes %s", ErrFileKind, f.Name, f.MediaType, strings.Join(s.fileKinds(), ", "))
	}
	media, err := checkedKind(f)
	if err != nil {
		return "", err
	}
	f.MediaType = media
	id, err := s.Driver.SendFile(ctx, t.room, t.agent, f)
	switch {
	case err == nil:
		return id, nil
	case errors.Is(err, ErrFileTooLarge), errors.Is(err, ErrFileKind):
		return "", err
	case IsRefused(err):
		return "", fmt.Errorf("the chat system refused the file: %w", err)
	}
	return "", fmt.Errorf("%w: %v", ErrChatDown, err)
}

// targetOf returns the session's room and agent, or nil.
func (s *Service) targetOf(name string) *agentSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	if task := s.tasks[name]; task != nil {
		return task.target
	}
	for _, t := range s.by {
		if t.session == name {
			return t
		}
	}
	return nil
}

func (s *Service) maxFile() int64 {
	if s.MaxFile > 0 {
		return int64(s.MaxFile)
	}
	return int64(DefaultMaxFile)
}

func (s *Service) fileKinds() []string {
	if len(s.FileKinds) > 0 {
		return s.FileKinds
	}
	return DefaultFileKinds
}

// sizeText writes a number of bytes as the size units do.
func sizeText(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}

// checkedKind returns the media type the file is sent under, after the bytes
// have been looked at: the kind a caller names is its own word, and a file
// whose bytes belong to another kind than the one it is called is refused.
// The type is the one that was named, without its parameters, when the bytes
// agree with it.
func checkedKind(f File) (string, error) {
	named, _, err := mime.ParseMediaType(f.MediaType)
	if err != nil {
		return "", fmt.Errorf("%w: %s is %q", ErrFileKind, f.Name, f.MediaType)
	}
	detected, _, _ := mime.ParseMediaType(http.DetectContentType(f.Data))
	if !agrees(named, detected) {
		return "", fmt.Errorf("%w: %s is called %s and its bytes are %s", ErrFileKind, f.Name, named, detected)
	}
	return named, nil
}

// agrees says whether bytes that sniff as detected can be of the named kind.
// Text is any of the kinds written in text; a picture, a PDF and anything
// else binary must be the kind the bytes are.
func agrees(named, detected string) bool {
	if strings.HasPrefix(detected, "text/") {
		return writtenInText(named)
	}
	return named == detected
}

// writtenInText says whether files of the media type are text.
func writtenInText(media string) bool {
	switch {
	case strings.HasPrefix(media, "text/"), strings.HasSuffix(media, "+json"), strings.HasSuffix(media, "+xml"):
		return true
	}
	switch media {
	case "application/json", "application/xml", "application/yaml", "application/x-yaml", "application/javascript", "application/toml":
		return true
	}
	return false
}

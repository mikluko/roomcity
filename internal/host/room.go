package host

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	roomcityv1 "sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1"
)

// roomServerName is the name the agent knows the host's tools by: goose shows
// "room: attach" and Claude Code mcp__room__attach.
const roomServerName = "room"

// maxFileRead is the most the host reads of a file to send; the room's own
// bound is the server's, which says why in its refusal.
const maxFileRead = 64 << 20

// sendTimeout bounds one file's way to the room.
const sendTimeout = 2 * time.Minute

// room is the MCP server the host gives its agent: the tools by which the
// agent speaks to the session's room. It serves on the loopback of the pod
// to whoever holds secret.
type room struct {
	workDir string
	secret  string
	send    func(context.Context, *roomcityv1.SendRequest) (string, error)
	srv     *http.Server
	url     string
}

// startRoom serves the room's tools on a loopback port until stop is called.
// send hands one file to the server and returns the room's event for it.
func startRoom(workDir string, send func(context.Context, *roomcityv1.SendRequest) (string, error)) (*room, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	r := &room{workDir: workDir, secret: hex.EncodeToString(b), send: send}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	r.url = "http://" + ln.Addr().String() + "/mcp"
	mux := http.NewServeMux()
	mux.Handle("/mcp", r.authorized(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return r.server() },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})))
	r.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = r.srv.Serve(ln) }()
	return r, nil
}

func (r *room) stop() { _ = r.srv.Close() }

// entry is the server as session/new gives it to an agent.
func (r *room) entry() (name, address, header string) {
	return roomServerName, r.url, "Bearer " + r.secret
}

func (r *room) authorized(next http.Handler) http.Handler {
	want := []byte("Bearer " + r.secret)
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if subtle.ConstantTimeCompare([]byte(req.Header.Get("Authorization")), want) != 1 {
			http.Error(w, "the room's tools are for its session's agent", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, req)
	})
}

type attachArgs struct {
	Path    string `json:"path" jsonschema:"the file to send, relative to your working directory or an absolute path inside it"`
	Caption string `json:"caption,omitempty" jsonschema:"what the owner reads with the file; optional"`
}

func (r *room) server() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: roomServerName, Version: "1"}, nil)
	mcp.AddTool(s, &mcp.Tool{
		Name: "attach",
		Description: "Send a file of your working directory to the room, where the owner reads this conversation: an image is shown in it, " +
			"anything else is a file the owner can download. Use it for a chart, a screenshot, a report or any file the owner should see; " +
			"the owner cannot open files in your pod. It answers when the room has the file, or says why it does not.",
	}, r.attach)
	return s
}

// attach is the tool attach: it reads the file, which must lie within the
// working directory, and hands it to the server.
func (r *room) attach(ctx context.Context, _ *mcp.CallToolRequest, in attachArgs) (*mcp.CallToolResult, any, error) {
	name, media, data, err := readInside(r.workDir, in.Path)
	if err != nil {
		return refused(err), nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	if _, err := r.send(ctx, &roomcityv1.SendRequest{Name: name, MediaType: media, Caption: in.Caption, Data: data}); err != nil {
		return refused(fmt.Errorf("the room did not take %s: %s", name, reason(err))), nil, nil
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("%s is in the room (%s, %d bytes).", name, media, len(data))}}}, nil, nil
}

func refused(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}
}

// reason is what the agent reads of a failed call: the server's own words
// when it gave them.
func reason(err error) string {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce.Message()
	}
	return err.Error()
}

// readInside returns the name, media type and content of the regular file at
// path, which is the working directory's own: a path outside it, by ".." or
// by a link, is refused.
func readInside(workDir, path string) (name, media string, data []byte, err error) {
	if path == "" {
		return "", "", nil, errors.New("no path given")
	}
	rel := path
	if filepath.IsAbs(path) {
		if rel, err = filepath.Rel(workDir, path); err != nil {
			return "", "", nil, fmt.Errorf("%s is outside your working directory", path)
		}
	}
	root, err := os.OpenRoot(workDir)
	if err != nil {
		return "", "", nil, fmt.Errorf("your working directory cannot be opened: %v", err)
	}
	defer func() { _ = root.Close() }()
	f, err := root.Open(rel)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", "", nil, fmt.Errorf("%s does not exist in your working directory", path)
		}
		return "", "", nil, fmt.Errorf("%s is outside your working directory or cannot be read", path)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return "", "", nil, err
	}
	switch {
	case !info.Mode().IsRegular():
		return "", "", nil, fmt.Errorf("%s is not a file", path)
	case info.Size() == 0:
		return "", "", nil, fmt.Errorf("%s is empty", path)
	case info.Size() > maxFileRead:
		return "", "", nil, fmt.Errorf("%s is %d MiB; the largest file that can be sent is %d MiB", path, info.Size()>>20, maxFileRead>>20)
	}
	data, err = io.ReadAll(io.LimitReader(f, maxFileRead+1))
	if err != nil {
		return "", "", nil, err
	}
	if len(data) > maxFileRead {
		return "", "", nil, fmt.Errorf("%s is larger than the %d MiB that can be sent", path, maxFileRead>>20)
	}
	name = filepath.Base(info.Name())
	media = mime.TypeByExtension(strings.ToLower(filepath.Ext(name)))
	if media == "" {
		media = http.DetectContentType(data)
	}
	if mt, _, perr := mime.ParseMediaType(media); perr == nil {
		media = mt
	}
	return name, media, data, nil
}

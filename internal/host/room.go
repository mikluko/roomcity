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
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"sarak.sh/mikluko/roomcity/internal/config"
	roomcityv1 "sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1"
	"sarak.sh/mikluko/roomcity/internal/roomtool"
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
	session string
	rpc     roomRPC
	tools   []string
	srv     *http.Server
	url     string
}

// roomRPC is what the room's tools call on the server: the session's side of
// HostService. The session's token is the client's.
type roomRPC interface {
	Send(context.Context, *connect.Request[roomcityv1.SendRequest]) (*connect.Response[roomcityv1.SendResponse], error)
	Dispatch(context.Context, *connect.Request[roomcityv1.DispatchRequest]) (*connect.Response[roomcityv1.DispatchResponse], error)
	Wait(context.Context, *connect.Request[roomcityv1.WaitRequest]) (*connect.Response[roomcityv1.WaitResponse], error)
	Claim(context.Context, *connect.Request[roomcityv1.ClaimRequest]) (*connect.Response[roomcityv1.ClaimResponse], error)
	Done(context.Context, *connect.Request[roomcityv1.DoneRequest]) (*connect.Response[roomcityv1.DoneResponse], error)
	Result(context.Context, *connect.Request[roomcityv1.ResultRequest]) (*connect.Response[roomcityv1.ResultResponse], error)
}

// roomToolNames is attach and the work tools that extra names, once each and in
// the order of workTools; a name that is no tool of the room is left out.
func roomToolNames(extra []string) []string {
	names := []string{"attach"}
	for _, t := range config.RoomTools {
		if slices.Contains(extra, t) {
			names = append(names, t)
		}
	}
	return names
}

// startRoom serves the room's tools, the names tools holds, on a loopback port
// until stop is called.
func startRoom(workDir, session string, rpc roomRPC, tools []string) (*room, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	r := &room{workDir: workDir, secret: hex.EncodeToString(b), session: session, rpc: rpc, tools: tools}
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
	for _, name := range r.tools {
		switch name {
		case "attach":
			mcp.AddTool(s, &mcp.Tool{
				Name: "attach",
				Description: "Send a file of your working directory to the room, where the owner reads this conversation: an image is shown in it, " +
					"anything else is a file the owner can download. Use it for a chart, a screenshot, a report or any file the owner should see; " +
					"the owner cannot open files in your pod. It answers when the room has the file, or says why it does not.",
			}, r.attach)
		case "dispatch":
			mcp.AddTool(s, &mcp.Tool{
				Name: "dispatch",
				Description: "Hand a task to a worker. Say in full what the worker is to do: it has nothing but what you write here. " +
					"It answers with the task's id, which wait and result take.",
			}, r.dispatch)
		case "wait":
			mcp.AddTool(s, &mcp.Tool{
				Name: "wait",
				Description: "Ask to be told when a task you dispatched is closed. The telling arrives later as a message to you, " +
					"so end your turn after asking if there is nothing else to do.",
			}, r.wait)
		case "result":
			mcp.AddTool(s, &mcp.Tool{
				Name: "result",
				Description: "Read how a task you dispatched stands and, once it is closed, its outcome and the worker's result. " +
					"A closed task's outcome is no-op (carried out, with a result and no change committed), shipped (carried out, with a commit and its branch), " +
					"blocked (the worker could not carry it out), or abandoned (failed: the server ended the worker's session before it gave a result; the server's reason follows).",
			}, r.result)
		case "claim":
			mcp.AddTool(s, &mcp.Tool{
				Name:        "claim",
				Description: "Take the task you were started for. When there is none, the session ends.",
			}, r.claim)
		case "done":
			mcp.AddTool(s, &mcp.Tool{
				Name: "done",
				Description: "Close the task you claimed with what came of it, for the one who dispatched it to read. " +
					"Set blocked when you could not carry out the task; completing it without a commit is not blocked. " +
					"For work that changed a repository and made a commit, give commit (the commit id, 7 to 40 hexadecimal digits) and a non-empty branch together; otherwise omit both. " +
					"A blocked task takes neither commit nor branch: put what prevented it in result. The session ends.",
			}, r.done)
		}
	}
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
	if _, err := r.rpc.Send(ctx, connect.NewRequest(&roomcityv1.SendRequest{Session: r.session, Name: name, MediaType: media, Caption: in.Caption, Data: data})); err != nil {
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

type taskArgs struct {
	Task string `json:"task" jsonschema:"the task's id, as dispatch gave it"`
}

type dispatchArgs struct {
	Task string `json:"task" jsonschema:"what the worker is to do, in full"`
}

type doneArgs struct {
	Result  string `json:"result" jsonschema:"what came of the task; if blocked, what prevented it"`
	Blocked bool   `json:"blocked,omitempty" jsonschema:"true only when you could not carry out the task; do not set it merely because no commit was made"`
	Commit  string `json:"commit,omitempty" jsonschema:"the id of the commit that satisfies the task, 7 to 40 hexadecimal digits; give branch with it, and use only for work that changed a repository; otherwise omit both commit and branch"`
	Branch  string `json:"branch,omitempty" jsonschema:"the non-empty branch that commit is on; give commit with it, and use only for work that changed a repository; otherwise omit both commit and branch"`
}

type noArgs struct{}

// said is a tool's answer that says text.
func said(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

// call bounds one call of the server.
func call(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, sendTimeout)
}

func (r *room) dispatch(ctx context.Context, _ *mcp.CallToolRequest, in dispatchArgs) (*mcp.CallToolResult, any, error) {
	if strings.TrimSpace(in.Task) == "" {
		return refused(errors.New("no task given")), nil, nil
	}
	ctx, cancel := call(ctx)
	defer cancel()
	res, err := r.rpc.Dispatch(ctx, connect.NewRequest(&roomcityv1.DispatchRequest{Session: r.session, Task: in.Task}))
	if err != nil {
		return refused(fmt.Errorf("the task was not handed to a worker: %s", reason(err))), nil, nil
	}
	return said(roomtool.DispatchAnswer(res.Msg.TaskId)), nil, nil
}

func (r *room) wait(ctx context.Context, _ *mcp.CallToolRequest, in taskArgs) (*mcp.CallToolResult, any, error) {
	ctx, cancel := call(ctx)
	defer cancel()
	if _, err := r.rpc.Wait(ctx, connect.NewRequest(&roomcityv1.WaitRequest{Session: r.session, TaskId: in.Task})); err != nil {
		return refused(fmt.Errorf("not waiting for task %s: %s", in.Task, reason(err))), nil, nil
	}
	return said("You will be told when task " + in.Task + " is closed."), nil, nil
}

func (r *room) result(ctx context.Context, _ *mcp.CallToolRequest, in taskArgs) (*mcp.CallToolResult, any, error) {
	ctx, cancel := call(ctx)
	defer cancel()
	res, err := r.rpc.Result(ctx, connect.NewRequest(&roomcityv1.ResultRequest{Session: r.session, TaskId: in.Task}))
	if err != nil {
		return refused(fmt.Errorf("no result for task %s: %s", in.Task, reason(err))), nil, nil
	}
	if !res.Msg.Closed {
		return said(fmt.Sprintf("Task %s is not done yet (%s).", in.Task, res.Msg.Status)), nil, nil
	}
	var outcome string
	switch res.Msg.Outcome {
	case "no-op":
		outcome = "no-op — carried out, with a result and no change committed"
	case "shipped":
		outcome = "shipped — carried out, with a commit and its branch recorded"
	case "blocked":
		outcome = "blocked — the worker could not carry out the task"
	case "abandoned":
		outcome = "abandoned — failed: the server ended the worker's session before it gave a result"
	default:
		if strings.TrimSpace(res.Msg.Result) == "" {
			return said("Task " + in.Task + " is closed, and the worker left nothing."), nil, nil
		}
		return said(res.Msg.Result), nil, nil
	}
	answer := fmt.Sprintf("Task %s is closed. Outcome: %s.", in.Task, outcome)
	if res.Msg.Outcome == "abandoned" {
		if strings.TrimSpace(res.Msg.Result) == "" {
			return said(answer + "\nThe server gave no reason."), nil, nil
		}
		return said(answer + "\nReason from the server:\n" + res.Msg.Result), nil, nil
	}
	if strings.TrimSpace(res.Msg.Result) == "" {
		return said(answer + "\nThe worker left no result."), nil, nil
	}
	return said(answer + "\nWorker's result:\n" + res.Msg.Result), nil, nil
}

func (r *room) claim(ctx context.Context, _ *mcp.CallToolRequest, _ noArgs) (*mcp.CallToolResult, any, error) {
	ctx, cancel := call(ctx)
	defer cancel()
	res, err := r.rpc.Claim(ctx, connect.NewRequest(&roomcityv1.ClaimRequest{Session: r.session}))
	if err != nil {
		return refused(fmt.Errorf("no task could be claimed: %s", reason(err))), nil, nil
	}
	if !res.Msg.Found {
		return said(roomtool.NoTaskAnswer), nil, nil
	}
	return said(roomtool.ClaimAnswer(res.Msg.TaskId, res.Msg.Text)), nil, nil
}

func isCommitID(s string) bool {
	if len(s) < 7 || len(s) > 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

func (r *room) done(ctx context.Context, _ *mcp.CallToolRequest, in doneArgs) (*mcp.CallToolResult, any, error) {
	if in.Blocked && (in.Commit != "" || in.Branch != "") {
		return refused(errors.New("a blocked task records no commit: put it in the result")), nil, nil
	}
	if !in.Blocked && (in.Commit == "") != (in.Branch == "") {
		return refused(errors.New("a commit and its branch are given together or not at all")), nil, nil
	}
	if in.Commit != "" && !isCommitID(in.Commit) {
		return refused(errors.New("commit is not an id of 7 to 40 hexadecimal digits: give the commit's id and its branch, or omit both")), nil, nil
	}
	if in.Branch != "" && strings.TrimSpace(in.Branch) == "" {
		return refused(errors.New("branch is empty after trimming: give the branch the commit is on, or omit both commit and branch")), nil, nil
	}
	ctx, cancel := call(ctx)
	defer cancel()
	req := &roomcityv1.DoneRequest{Session: r.session, Result: in.Result, Blocked: in.Blocked, Commit: in.Commit, Branch: in.Branch}
	if _, err := r.rpc.Done(ctx, connect.NewRequest(req)); err != nil {
		return refused(fmt.Errorf("the task was not closed: %s", reason(err))), nil, nil
	}
	return said("The task is closed; this session is ending."), nil, nil
}

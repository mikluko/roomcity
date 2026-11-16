package host

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	roomcityv1 "sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1"
)

type bearer struct{ header string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", b.header)
	return http.DefaultTransport.RoundTrip(r)
}

// fakeRoomRPC is the server the room's tools call: it keeps what it is sent
// and answers as it is told.
type fakeRoomRPC struct {
	mu         sync.Mutex
	sent       []*roomcityv1.SendRequest
	dispatched []string
	waited     []string
	done       []*roomcityv1.DoneRequest
	err        error // what every call answers with
	closed     bool  // whether Result says the task is closed
	found      bool  // whether Claim finds a task
	status     string
	result     string
	outcome    string
}

func (f *fakeRoomRPC) Send(_ context.Context, r *connect.Request[roomcityv1.SendRequest]) (*connect.Response[roomcityv1.SendResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, r.Msg)
	return connect.NewResponse(&roomcityv1.SendResponse{Event: "$event"}), f.err
}

func (f *fakeRoomRPC) Dispatch(_ context.Context, r *connect.Request[roomcityv1.DispatchRequest]) (*connect.Response[roomcityv1.DispatchResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dispatched = append(f.dispatched, r.Msg.Session+":"+r.Msg.Task)
	return connect.NewResponse(&roomcityv1.DispatchResponse{TaskId: "sa-9"}), f.err
}

func (f *fakeRoomRPC) Wait(_ context.Context, r *connect.Request[roomcityv1.WaitRequest]) (*connect.Response[roomcityv1.WaitResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.waited = append(f.waited, r.Msg.TaskId)
	return connect.NewResponse(&roomcityv1.WaitResponse{}), f.err
}

func (f *fakeRoomRPC) Claim(context.Context, *connect.Request[roomcityv1.ClaimRequest]) (*connect.Response[roomcityv1.ClaimResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return connect.NewResponse(&roomcityv1.ClaimResponse{Found: f.found, TaskId: "sa-9", Text: "Fix the bug\nIt crashes."}), f.err
}

func (f *fakeRoomRPC) Done(_ context.Context, r *connect.Request[roomcityv1.DoneRequest]) (*connect.Response[roomcityv1.DoneResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.done = append(f.done, r.Msg)
	return connect.NewResponse(&roomcityv1.DoneResponse{}), f.err
}

func (f *fakeRoomRPC) Result(context.Context, *connect.Request[roomcityv1.ResultRequest]) (*connect.Response[roomcityv1.ResultResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return connect.NewResponse(&roomcityv1.ResultResponse{Closed: f.closed, Status: f.status, Result: f.result, Outcome: f.outcome}), f.err
}

// roomSession serves the room with the tools over dir and returns an MCP
// client session of it and the server it calls.
func roomSessionWith(t *testing.T, dir string, rpc *fakeRoomRPC, tools ...string) *mcp.ClientSession {
	t.Helper()
	r, err := startRoom(dir, "s", rpc, roomToolNames(tools))
	require.NoError(t, err)
	t.Cleanup(r.stop)
	_, address, header := r.entry()
	client := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "1"}, nil)
	cs, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: address, HTTPClient: &http.Client{Transport: bearer{header}}, DisableStandaloneSSE: true,
	}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// roomSession serves the room with attach only, over dir; answer is what the
// server answers with. It returns the client session and the requests the
// server was handed.
func roomSession(t *testing.T, dir string, answer error) (*mcp.ClientSession, *[]*roomcityv1.SendRequest) {
	t.Helper()
	rpc := &fakeRoomRPC{err: answer}
	cs := roomSessionWith(t, dir, rpc)
	return cs, &rpc.sent
}

func attach(t *testing.T, cs *mcp.ClientSession, path, caption string) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "attach", Arguments: map[string]any{"path": path, "caption": caption}})
	require.NoError(t, err)
	return res
}

func text(res *mcp.CallToolResult) string {
	return res.Content[0].(*mcp.TextContent).Text
}

func TestTheRoomOffersAttachAndSendsTheFileWithItsCaption(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "chart.png"), []byte("\x89PNG\r\n\x1a\nrest"), 0o644))
	cs, sent := roomSession(t, dir, nil)

	tools, err := cs.ListTools(context.Background(), nil)
	require.NoError(t, err)
	require.Len(t, tools.Tools, 1)
	require.Equal(t, "attach", tools.Tools[0].Name)

	res := attach(t, cs, "chart.png", "the weekly numbers")

	require.False(t, res.IsError, text(res))
	require.Len(t, *sent, 1)
	got := (*sent)[0]
	require.Equal(t, "chart.png", got.Name)
	require.Equal(t, "image/png", got.MediaType)
	require.Equal(t, "the weekly numbers", got.Caption)
	require.Equal(t, "\x89PNG\r\n\x1a\nrest", string(got.Data))
}

func TestAnAbsolutePathInsideTheWorkingDirectoryIsTaken(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "out"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "out", "report.txt"), []byte("hello"), 0o644))
	cs, sent := roomSession(t, dir, nil)

	res := attach(t, cs, filepath.Join(dir, "out", "report.txt"), "")

	require.False(t, res.IsError, text(res))
	require.Equal(t, "text/plain", (*sent)[0].MediaType)
}

func TestWhatLiesOutsideTheWorkingDirectoryOrIsNoFileIsRefusedWithAReason(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("s"), 0o644))
	require.NoError(t, os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(dir, "link.txt")))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "empty"), nil, 0o644))
	cs, sent := roomSession(t, dir, nil)

	for path, want := range map[string]string{
		"../" + filepath.Base(outside) + "/secret.txt": "outside your working directory",
		filepath.Join(outside, "secret.txt"):           "outside your working directory",
		"link.txt":                                     "outside your working directory",
		"missing.png":                                  "does not exist",
		"sub":                                          "is not a file",
		"empty":                                        "is empty",
		"":                                             "no path",
	} {
		res := attach(t, cs, path, "")
		require.True(t, res.IsError, path)
		require.Contains(t, text(res), want, path)
	}
	require.Empty(t, *sent, "nothing went to the server")
}

func TestTheServersReasonForRefusingAFileReachesTheAgent(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "big.bin"), []byte("x"), 0o644))
	cs, _ := roomSession(t, dir, connect.NewError(connect.CodeInvalidArgument, errors.New("the file is 31 MiB; the room takes 10 MiB")))

	res := attach(t, cs, "big.bin", "")

	require.True(t, res.IsError)
	require.Contains(t, text(res), "the file is 31 MiB; the room takes 10 MiB")
	require.NotContains(t, text(res), "invalid_argument")
}

func TestTheRoomServesOnlyThoseWhoHoldItsSecret(t *testing.T) {
	r, err := startRoom(t.TempDir(), "s", &fakeRoomRPC{}, []string{"attach"})
	require.NoError(t, err)
	defer r.stop()
	_, address, header := r.entry()

	for name, h := range map[string]string{"none": "", "wrong": "Bearer nope", "no scheme": header[len("Bearer "):]} {
		req, _ := http.NewRequest(http.MethodPost, address, nil)
		if h != "" {
			req.Header.Set("Authorization", h)
		}
		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err, name)
		_ = res.Body.Close()
		require.Equal(t, http.StatusUnauthorized, res.StatusCode, name)
	}
}

func toolNames(t *testing.T, cs *mcp.ClientSession) []string {
	t.Helper()
	tools, err := cs.ListTools(context.Background(), nil)
	require.NoError(t, err)
	var names []string
	for _, tl := range tools.Tools {
		names = append(names, tl.Name)
	}
	return names
}

func callTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	require.NoError(t, err)
	return res
}

func TestARoomOffersOnlyTheToolsItsSessionHas(t *testing.T) {
	for name, tools := range map[string][]string{
		"none":     nil,
		"foreman":  {"dispatch", "wait", "result"},
		"worker":   {"claim", "done"},
		"unknown":  {"spend", "dispatch", "dispatch"},
		"all five": {"done", "claim", "result", "wait", "dispatch"},
	} {
		cs := roomSessionWith(t, t.TempDir(), &fakeRoomRPC{}, tools...)
		want := roomToolNames(tools)
		require.ElementsMatch(t, want, toolNames(t, cs), name)
		require.Contains(t, want, "attach", name)
		require.NotContains(t, want, "spend", name)
	}
}

func TestDoneAndResultSchemasTellTheAgentHowToUseThem(t *testing.T) {
	cs := roomSessionWith(t, t.TempDir(), &fakeRoomRPC{}, "done", "result")
	tools, err := cs.ListTools(context.Background(), nil)
	require.NoError(t, err)
	byName := make(map[string]*mcp.Tool, len(tools.Tools))
	for _, tool := range tools.Tools {
		byName[tool.Name] = tool
	}

	type schema struct {
		Type       string `json:"type"`
		Properties map[string]struct {
			Type        string `json:"type"`
			Description string `json:"description"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	schemaOf := func(tool *mcp.Tool) schema {
		t.Helper()
		raw, err := json.Marshal(tool.InputSchema)
		require.NoError(t, err)
		var got schema
		require.NoError(t, json.Unmarshal(raw, &got))
		return got
	}

	done := schemaOf(byName["done"])
	require.Equal(t, "object", done.Type)
	require.ElementsMatch(t, []string{"result"}, done.Required)
	wantDone := map[string]struct{ kind, description string }{
		"result":  {"string", "what came of the task; if blocked, what prevented it"},
		"blocked": {"boolean", "true only when you could not carry out the task; do not set it merely because no commit was made"},
		"commit":  {"string", "the id of the commit that satisfies the task, 7 to 40 hexadecimal digits; give branch with it, and use only for work that changed a repository; otherwise omit both commit and branch"},
		"branch":  {"string", "the non-empty branch that commit is on; give commit with it, and use only for work that changed a repository; otherwise omit both commit and branch"},
	}
	require.Len(t, done.Properties, len(wantDone))
	for name, want := range wantDone {
		require.Equal(t, want.kind, done.Properties[name].Type, name)
		require.Equal(t, want.description, done.Properties[name].Description, name)
	}
	require.Contains(t, byName["done"].Description, "completing it without a commit is not blocked")
	require.Contains(t, byName["done"].Description, "A blocked task takes neither commit nor branch: put what prevented it in result.")

	result := schemaOf(byName["result"])
	require.Equal(t, "object", result.Type)
	require.ElementsMatch(t, []string{"task"}, result.Required)
	require.Len(t, result.Properties, 1)
	require.Equal(t, "string", result.Properties["task"].Type)
	for _, outcome := range []string{"no-op", "shipped", "blocked", "abandoned"} {
		require.Contains(t, byName["result"].Description, outcome)
	}
	require.Contains(t, byName["result"].Description, "abandoned (failed: the server ended the worker's session before it gave a result; the server's reason follows)")
}

func TestTheForemanHandsATaskToAWorkerAndAsksHowItStands(t *testing.T) {
	rpc := &fakeRoomRPC{status: "in_progress"}
	cs := roomSessionWith(t, t.TempDir(), rpc, "dispatch", "wait", "result")

	res := callTool(t, cs, "dispatch", map[string]any{"task": "Fix the bug in the parser"})
	require.False(t, res.IsError, text(res))
	require.Contains(t, text(res), "sa-9")
	require.Equal(t, []string{"s:Fix the bug in the parser"}, rpc.dispatched)

	res = callTool(t, cs, "wait", map[string]any{"task": "sa-9"})
	require.False(t, res.IsError, text(res))
	require.Equal(t, []string{"sa-9"}, rpc.waited)

	res = callTool(t, cs, "result", map[string]any{"task": "sa-9"})
	require.Equal(t, "Task sa-9 is not done yet (in_progress).", text(res))

	res = callTool(t, cs, "dispatch", map[string]any{"task": "  "})
	require.True(t, res.IsError, "an empty task is no task")
	require.Len(t, rpc.dispatched, 1)
}

func TestResultSaysHowAClosedTaskEndedBeforeWhatTheWorkerLeft(t *testing.T) {
	for name, tc := range map[string]struct {
		outcome string
		result  string
		want    string
	}{
		"no-op": {
			outcome: "no-op",
			result:  "Explained why no change was needed.",
			want:    "Task sa-9 is closed. Outcome: no-op — carried out, with a result and no change committed.\nWorker's result:\nExplained why no change was needed.",
		},
		"shipped": {
			outcome: "shipped",
			result:  "Fixed in 3 lines.",
			want:    "Task sa-9 is closed. Outcome: shipped — carried out, with a commit and its branch recorded.\nWorker's result:\nFixed in 3 lines.",
		},
		"blocked": {
			outcome: "blocked",
			result:  "Needs owner approval.",
			want:    "Task sa-9 is closed. Outcome: blocked — the worker could not carry out the task.\nWorker's result:\nNeeds owner approval.",
		},
		"abandoned": {
			outcome: "abandoned",
			result:  "ended without a result: timed out after 30m0s",
			want:    "Task sa-9 is closed. Outcome: abandoned — failed: the server ended the worker's session before it gave a result.\nReason from the server:\nended without a result: timed out after 30m0s",
		},
	} {
		t.Run(name, func(t *testing.T) {
			rpc := &fakeRoomRPC{closed: true, status: "closed", outcome: tc.outcome, result: tc.result}
			cs := roomSessionWith(t, t.TempDir(), rpc, "result")

			res := callTool(t, cs, "result", map[string]any{"task": "sa-9"})

			require.False(t, res.IsError, text(res))
			require.Equal(t, tc.want, text(res))
		})
	}
}

func TestResultWithoutAKnownOutcomeIsLeftAsItWas(t *testing.T) {
	for name, tc := range map[string]struct {
		outcome string
		result  string
		want    string
	}{
		"no outcome":       {result: "Fixed in 3 lines.", want: "Fixed in 3 lines."},
		"unknown outcome":  {outcome: "future", result: "Fixed in 3 lines.", want: "Fixed in 3 lines."},
		"nothing recorded": {want: "Task sa-9 is closed, and the worker left nothing."},
	} {
		t.Run(name, func(t *testing.T) {
			rpc := &fakeRoomRPC{closed: true, status: "closed", outcome: tc.outcome, result: tc.result}
			cs := roomSessionWith(t, t.TempDir(), rpc, "result")
			require.Equal(t, tc.want, text(callTool(t, cs, "result", map[string]any{"task": "sa-9"})))
		})
	}
}

func TestTheWorkerClaimsItsTaskAndClosesItWithWhatCameOfIt(t *testing.T) {
	rpc := &fakeRoomRPC{found: true}
	cs := roomSessionWith(t, t.TempDir(), rpc, "claim", "done")

	res := callTool(t, cs, "claim", nil)
	require.False(t, res.IsError, text(res))
	require.Equal(t, "Task sa-9:\nFix the bug\nIt crashes.", text(res))

	res = callTool(t, cs, "done", map[string]any{"result": "Fixed in 3 lines."})
	require.False(t, res.IsError, text(res))
	require.Equal(t, []*roomcityv1.DoneRequest{{Session: "s", Result: "Fixed in 3 lines."}}, rpc.done)

	rpc.found = false
	require.Contains(t, text(callTool(t, cs, "claim", nil)), "no task for you")
}

func TestDonePassesBlockedToTheServer(t *testing.T) {
	rpc := &fakeRoomRPC{}
	cs := roomSessionWith(t, t.TempDir(), rpc, "done")

	res := callTool(t, cs, "done", map[string]any{"result": "Needs owner approval.", "blocked": true})

	require.False(t, res.IsError, text(res))
	require.Equal(t, []*roomcityv1.DoneRequest{{Session: "s", Result: "Needs owner approval.", Blocked: true}}, rpc.done)
}

func TestDoneAcceptsFullAndAbbreviatedCommitIDs(t *testing.T) {
	for name, commit := range map[string]string{
		"abbreviated": "abc1234",
		"full":        "0123456789abcdef0123456789abcdef01234567",
	} {
		t.Run(name, func(t *testing.T) {
			rpc := &fakeRoomRPC{}
			cs := roomSessionWith(t, t.TempDir(), rpc, "done")

			res := callTool(t, cs, "done", map[string]any{"result": "Fixed in 3 lines.", "commit": commit, "branch": "fix-parser"})

			require.False(t, res.IsError, text(res))
			require.Equal(t, []*roomcityv1.DoneRequest{{Session: "s", Result: "Fixed in 3 lines.", Commit: commit, Branch: "fix-parser"}}, rpc.done)
		})
	}
}

func TestDoneRefusesACommitOrBranchWithoutTheOther(t *testing.T) {
	for name, args := range map[string]map[string]any{
		"commit without branch": {"result": "Fixed.", "commit": "abc1234"},
		"branch without commit": {"result": "Fixed.", "branch": "fix-parser"},
	} {
		t.Run(name, func(t *testing.T) {
			rpc := &fakeRoomRPC{}
			cs := roomSessionWith(t, t.TempDir(), rpc, "done")

			res := callTool(t, cs, "done", args)

			require.True(t, res.IsError)
			require.Equal(t, "a commit and its branch are given together or not at all", text(res))
			require.Empty(t, rpc.done)
		})
	}
}

func TestDoneRefusesACommitOrBranchOnABlockedTask(t *testing.T) {
	for name, args := range map[string]map[string]any{
		"commit and branch": {"result": "Needs owner approval.", "blocked": true, "commit": "abc1234", "branch": "fix-parser"},
		"commit only":       {"result": "Needs owner approval.", "blocked": true, "commit": "abc1234"},
		"branch only":       {"result": "Needs owner approval.", "blocked": true, "branch": "fix-parser"},
	} {
		t.Run(name, func(t *testing.T) {
			rpc := &fakeRoomRPC{}
			cs := roomSessionWith(t, t.TempDir(), rpc, "done")

			res := callTool(t, cs, "done", args)

			require.True(t, res.IsError)
			require.Equal(t, "a blocked task records no commit: put it in the result", text(res))
			require.Empty(t, rpc.done)
		})
	}
}

func TestDoneRefusesACommitThatIsNotAnID(t *testing.T) {
	for name, commit := range map[string]string{
		"none":            "none",
		"too short":       "abc123",
		"too long":        "0123456789abcdef0123456789abcdef012345678",
		"non-hexadecimal": "abcdefg",
	} {
		t.Run(name, func(t *testing.T) {
			rpc := &fakeRoomRPC{}
			cs := roomSessionWith(t, t.TempDir(), rpc, "done")

			res := callTool(t, cs, "done", map[string]any{"result": "Fixed.", "commit": commit, "branch": "fix-parser"})

			require.True(t, res.IsError)
			require.Equal(t, "commit is not an id of 7 to 40 hexadecimal digits: give the commit's id and its branch, or omit both", text(res))
			require.Empty(t, rpc.done)
		})
	}
}

func TestDoneRefusesABranchThatIsEmptyAfterTrimming(t *testing.T) {
	rpc := &fakeRoomRPC{}
	cs := roomSessionWith(t, t.TempDir(), rpc, "done")

	res := callTool(t, cs, "done", map[string]any{"result": "Fixed.", "commit": "abc1234", "branch": " \t "})

	require.True(t, res.IsError)
	require.Equal(t, "branch is empty after trimming: give the branch the commit is on, or omit both commit and branch", text(res))
	require.Empty(t, rpc.done)
}

func TestTheServersRefusalOfAWorkToolReachesTheAgentInItsWords(t *testing.T) {
	rpc := &fakeRoomRPC{err: connect.NewError(connect.CodePermissionDenied, errors.New("the agent has no tool dispatch"))}
	cs := roomSessionWith(t, t.TempDir(), rpc, "dispatch")

	res := callTool(t, cs, "dispatch", map[string]any{"task": "x"})

	require.True(t, res.IsError)
	require.Contains(t, text(res), "the agent has no tool dispatch")
}

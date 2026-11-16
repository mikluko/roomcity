package server_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	roomcityv1 "sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1"
	"sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1/roomcityv1connect"
	"sarak.sh/mikluko/roomcity/internal/server"
	"sarak.sh/mikluko/roomcity/internal/session"
	"sarak.sh/mikluko/roomcity/internal/sessionauth"
)

// handing records what it is asked and answers as its fields say.
type handing struct {
	calls []string
	err   error
}

func (h *handing) Dispatch(_ context.Context, s, task string) (string, error) {
	h.calls = append(h.calls, "dispatch "+s+" "+task)
	return "bd-7", h.err
}

func (h *handing) Wait(_ context.Context, s, id string) error {
	h.calls = append(h.calls, "wait "+s+" "+id)
	return h.err
}

func (h *handing) Result(_ context.Context, s, id string) (bool, string, string, error) {
	h.calls = append(h.calls, "result "+s+" "+id)
	return true, "closed", "nine", h.err
}

func (h *handing) Claim(_ context.Context, s string) (string, string, bool, error) {
	h.calls = append(h.calls, "claim "+s)
	return "bd-7", "count the words", true, h.err
}

func (h *handing) Done(_ context.Context, s, result string) error {
	h.calls = append(h.calls, "done "+s+" "+result)
	return h.err
}

type workFixture struct {
	work    *handing
	foreman roomcityv1connect.HostServiceClient
	worker  roomcityv1connect.HostServiceClient
	anon    roomcityv1connect.HostServiceClient
}

func newWorkFixture(t *testing.T) *workFixture {
	t.Helper()
	ctx := context.Background()
	reg, err := session.Open(ctx, filepath.Join(t.TempDir(), "r.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = reg.Close() })
	foreman, err := reg.Start(ctx, "foreman-1", "foreman", session.AlwaysOn)
	require.NoError(t, err)
	worker, err := reg.Start(ctx, "worker-1", "worker", session.Task)
	require.NoError(t, err)

	f := &workFixture{work: &handing{}}
	tools := map[string][]string{"foreman": {"dispatch", "wait", "result"}, "worker": {"claim", "done"}}
	s := server.New()
	s.Handle(roomcityv1connect.NewHostServiceHandler(&server.Host{Registry: reg, Work: f.work, Tools: func(a string) []string { return tools[a] }}))
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	f.foreman = roomcityv1connect.NewHostServiceClient(ts.Client(), ts.URL, sessionauth.Client(foreman))
	f.worker = roomcityv1connect.NewHostServiceClient(ts.Client(), ts.URL, sessionauth.Client(worker))
	f.anon = roomcityv1connect.NewHostServiceClient(ts.Client(), ts.URL)
	return f
}

func TestTheForemanDispatchesAndWaitsAndAWorkerClaimsAndIsDone(t *testing.T) {
	f := newWorkFixture(t)
	ctx := context.Background()

	d, err := f.foreman.Dispatch(ctx, connect.NewRequest(&roomcityv1.DispatchRequest{Session: "foreman-1", Task: "count"}))
	require.NoError(t, err)
	require.Equal(t, "bd-7", d.Msg.TaskId)
	_, err = f.foreman.Wait(ctx, connect.NewRequest(&roomcityv1.WaitRequest{Session: "foreman-1", TaskId: "bd-7"}))
	require.NoError(t, err)

	r, err := f.foreman.Result(ctx, connect.NewRequest(&roomcityv1.ResultRequest{Session: "foreman-1", TaskId: "bd-7"}))
	require.NoError(t, err)
	require.True(t, r.Msg.Closed)
	require.Equal(t, "closed", r.Msg.Status)
	require.Equal(t, "nine", r.Msg.Result)

	c, err := f.worker.Claim(ctx, connect.NewRequest(&roomcityv1.ClaimRequest{Session: "worker-1"}))
	require.NoError(t, err)
	require.True(t, c.Msg.Found)
	require.Equal(t, "bd-7", c.Msg.TaskId)
	require.Equal(t, "count the words", c.Msg.Text)
	_, err = f.worker.Done(ctx, connect.NewRequest(&roomcityv1.DoneRequest{Session: "worker-1", Result: "nine"}))
	require.NoError(t, err)

	require.Equal(t, []string{"dispatch foreman-1 count", "wait foreman-1 bd-7", "result foreman-1 bd-7", "claim worker-1", "done worker-1 nine"}, f.work.calls)
}

func TestAToolIsForTheSessionsOfTheAgentThatHasItAndNoOther(t *testing.T) {
	f := newWorkFixture(t)
	ctx := context.Background()

	_, err := f.worker.Dispatch(ctx, connect.NewRequest(&roomcityv1.DispatchRequest{Session: "worker-1", Task: "x"}))
	require.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err))
	require.ErrorContains(t, err, "the agent worker has no tool dispatch")
	_, err = f.foreman.Claim(ctx, connect.NewRequest(&roomcityv1.ClaimRequest{Session: "foreman-1"}))
	require.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err))
	_, err = f.foreman.Done(ctx, connect.NewRequest(&roomcityv1.DoneRequest{Session: "foreman-1", Result: "x"}))
	require.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err))
	_, err = f.worker.Result(ctx, connect.NewRequest(&roomcityv1.ResultRequest{Session: "worker-1", TaskId: "bd-7"}))
	require.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err))
	_, err = f.worker.Wait(ctx, connect.NewRequest(&roomcityv1.WaitRequest{Session: "worker-1", TaskId: "bd-7"}))
	require.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err))

	// Not another session's token, either: the worker's is not the foreman's.
	_, err = f.worker.Dispatch(ctx, connect.NewRequest(&roomcityv1.DispatchRequest{Session: "foreman-1", Task: "x"}))
	require.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err))
	_, err = f.anon.Dispatch(ctx, connect.NewRequest(&roomcityv1.DispatchRequest{Session: "foreman-1", Task: "x"}))
	require.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err))
	require.Empty(t, f.work.calls)
}

func TestWhatGcRefusesIsSaidToTheAgentAsIs(t *testing.T) {
	f := newWorkFixture(t)
	f.work.err = errors.New("gc sling failed (exit 1): no such pool")
	_, err := f.foreman.Dispatch(context.Background(), connect.NewRequest(&roomcityv1.DispatchRequest{Session: "foreman-1", Task: "x"}))
	require.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err))
	require.ErrorContains(t, err, "gc sling failed (exit 1): no such pool")

	_, err = f.foreman.Dispatch(context.Background(), connect.NewRequest(&roomcityv1.DispatchRequest{Session: "foreman-1"}))
	require.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err))
}

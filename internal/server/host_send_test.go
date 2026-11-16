package server_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	roomcityv1 "sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1"
	"sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1/roomcityv1connect"
	"sarak.sh/mikluko/roomcity/internal/server"
	"sarak.sh/mikluko/roomcity/internal/session"
	"sarak.sh/mikluko/roomcity/internal/sessionauth"
)

// room is a chat side that keeps what it is sent, or refuses with err.
type room struct {
	got []server.File
	err error
}

func (r *room) SendFile(_ context.Context, sess string, f server.File) (string, error) {
	if r.err != nil {
		return "", r.err
	}
	r.got = append(r.got, f)
	return fmt.Sprintf("$event%d:%s", len(r.got), sess), nil
}

type sendFixture struct {
	reg    *session.Registry
	token  string
	client roomcityv1connect.HostServiceClient
	anon   roomcityv1connect.HostServiceClient
	url    string
	room   *room
}

func newSendFixture(t *testing.T, maxFile int64, withRoom bool) *sendFixture {
	t.Helper()
	ctx := context.Background()
	reg, err := session.Open(ctx, filepath.Join(t.TempDir(), "r.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = reg.Close() })
	token, err := reg.Start(ctx, "s", "host", session.AlwaysOn)
	require.NoError(t, err)

	f := &sendFixture{reg: reg, token: token, room: &room{}}
	h := &server.Host{Registry: reg, MaxFile: maxFile}
	if withRoom {
		h.Files = f.room
	}
	s := server.New()
	s.Handle(roomcityv1connect.NewHostServiceHandler(h))
	ts := httptest.NewServer(server.LimitSend(s.Handler(), reg, maxFile))
	t.Cleanup(ts.Close)
	f.url = ts.URL
	f.client = roomcityv1connect.NewHostServiceClient(ts.Client(), ts.URL, sessionauth.Client(token))
	f.anon = roomcityv1connect.NewHostServiceClient(ts.Client(), ts.URL)
	return f
}

func send(f *sendFixture, req *roomcityv1.SendRequest) (*roomcityv1.SendResponse, error) {
	res, err := f.client.Send(context.Background(), connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

func TestASessionsFileGoesToTheRoomAndIsRecordedWithoutItsBytes(t *testing.T) {
	f := newSendFixture(t, 1<<20, true)
	res, err := send(f, &roomcityv1.SendRequest{Session: "s", Name: "chart.png", MediaType: "image/png", Caption: "the counts", Data: []byte("pngbytes")})
	require.NoError(t, err)
	require.Equal(t, "$event1:s", res.Event)
	require.Equal(t, []server.File{{Name: "chart.png", MediaType: "image/png", Caption: "the counts", Data: []byte("pngbytes")}}, f.room.got)

	evs, err := f.reg.Tail(context.Background(), "s", 5)
	require.NoError(t, err)
	file, ok := evs[len(evs)-1].(session.File)
	require.True(t, ok, "the last event is the file's record: %T", evs[len(evs)-1])
	require.Equal(t, "chart.png", file.Name)
	require.Equal(t, "image/png", file.MediaType)
	require.Equal(t, int64(8), file.Size)
	require.Equal(t, "$event1:s", file.Event)
	require.False(t, file.Time.IsZero())
}

func TestSendRefusesWhatItCannotPlaceWithTheCodeAndReasonTheAgentReads(t *testing.T) {
	f := newSendFixture(t, 100, true)
	ok := &roomcityv1.SendRequest{Session: "s", Name: "a.txt", MediaType: "text/plain", Data: []byte("x")}
	with := func(edit func(r *roomcityv1.SendRequest)) *roomcityv1.SendRequest {
		r := &roomcityv1.SendRequest{Session: ok.Session, Name: ok.Name, MediaType: ok.MediaType, Data: ok.Data}
		edit(r)
		return r
	}
	for name, tc := range map[string]struct {
		req  *roomcityv1.SendRequest
		code connect.Code
		msg  string
	}{
		"no bytes":           {with(func(r *roomcityv1.SendRequest) { r.Data = nil }), connect.CodeInvalidArgument, "empty"},
		"too large":          {with(func(r *roomcityv1.SendRequest) { r.Data = bytes.Repeat([]byte("x"), 101) }), connect.CodeInvalidArgument, "at most 100"},
		"no name":            {with(func(r *roomcityv1.SendRequest) { r.Name = "" }), connect.CodeInvalidArgument, "not the name of a file"},
		"a path":             {with(func(r *roomcityv1.SendRequest) { r.Name = "../etc/passwd" }), connect.CodeInvalidArgument, "without a directory"},
		"a directory's name": {with(func(r *roomcityv1.SendRequest) { r.Name = ".." }), connect.CodeInvalidArgument, "not the name of a file"},
	} {
		_, err := send(f, tc.req)
		require.Equal(t, tc.code, connect.CodeOf(err), name)
		require.ErrorContains(t, err, tc.msg, name)
	}
	require.Empty(t, f.room.got, "nothing of it reached the room")

	// What the chat side refuses, by its own errors.
	for name, tc := range map[string]struct {
		err  error
		code connect.Code
	}{
		"too large for the room": {fmt.Errorf("%w: 12 bytes, the room takes 10", server.ErrFileTooLarge), connect.CodeInvalidArgument},
		"a kind it does not":     {fmt.Errorf("%w: video/mp4", server.ErrFileKind), connect.CodeInvalidArgument},
		"no room":                {fmt.Errorf("%w: it is a task's", server.ErrNoRoom), connect.CodeFailedPrecondition},
		"chat down":              {fmt.Errorf("%w: connection refused", server.ErrChatDown), connect.CodeUnavailable},
		"anything else":          {errors.New("boom"), connect.CodeInternal},
	} {
		f.room.err = tc.err
		_, err := send(f, ok)
		require.Equal(t, tc.code, connect.CodeOf(err), name)
		require.ErrorContains(t, err, tc.err.Error(), "the agent reads why")
	}

	evs, err := f.reg.Tail(context.Background(), "s", 5)
	require.NoError(t, err)
	for _, e := range evs {
		_, isFile := e.(session.File)
		require.False(t, isFile, "a file that was refused is not recorded")
	}
}

func TestASessionWithNoChatSideGetsUnavailable(t *testing.T) {
	f := newSendFixture(t, 100, false)
	_, err := send(f, &roomcityv1.SendRequest{Session: "s", Name: "a.txt", Data: []byte("x")})
	require.Equal(t, connect.CodeUnavailable, connect.CodeOf(err))
	require.ErrorContains(t, err, "no chat system")
}

func TestSendKnowsTheSessionByItsToken(t *testing.T) {
	f := newSendFixture(t, 100, true)
	req := &roomcityv1.SendRequest{Session: "s", Name: "a.txt", Data: []byte("x")}

	_, err := f.anon.Send(context.Background(), connect.NewRequest(req))
	require.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err))

	// Another session's token, or a token of an ended session, is not the session's.
	other, err := f.reg.Start(context.Background(), "t", "worker", session.Task)
	require.NoError(t, err)
	otherClient := roomcityv1connect.NewHostServiceClient(http.DefaultClient, f.url, sessionauth.Client(other))
	_, err = otherClient.Send(context.Background(), connect.NewRequest(req))
	require.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err))
	require.Empty(t, f.room.got)

	require.NoError(t, f.reg.End(context.Background(), "s"))
	_, err = f.client.Send(context.Background(), connect.NewRequest(req))
	require.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err))
}

func TestTheBodyOfASendIsNotReadForACallerThatIsNoSessionNorBeyondTheLimit(t *testing.T) {
	f := newSendFixture(t, 300, true)
	post := func(token string, body []byte) *http.Response {
		req, err := http.NewRequest(http.MethodPost, f.url+roomcityv1connect.HostServiceSendProcedure, bytes.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/proto")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		t.Cleanup(func() { _ = res.Body.Close() })
		return res
	}
	big := bytes.Repeat([]byte("x"), 1<<20)

	require.Equal(t, http.StatusUnauthorized, post("", big).StatusCode, "no token: refused without the body")
	require.Equal(t, http.StatusUnauthorized, post("not-a-token", big).StatusCode, "not a session's token")
	require.Equal(t, http.StatusRequestEntityTooLarge, post(f.token, big).StatusCode, "past the limit, from a session")
}

func TestLimitSendPassesWhatIsNotASend(t *testing.T) {
	var reached bool
	h := server.LimitSend(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }), noTokens{}, 100)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, roomcityv1connect.HostServiceReportProcedure, strings.NewReader("{}")))
	require.True(t, reached, "no token, and still not the wrapper's to refuse")
}

type noTokens struct{}

func (noTokens) KnownToken(string) bool { return false }

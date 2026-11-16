package server_test

import (
	"bytes"
	"context"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	roomcityv1 "sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1"
	"sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1/roomcityv1connect"
	"sarak.sh/mikluko/roomcity/internal/server"
	"sarak.sh/mikluko/roomcity/internal/session"
	"sarak.sh/mikluko/roomcity/internal/sessionauth"
)

type hostFixture struct {
	reg    *session.Registry
	token  string
	client roomcityv1connect.HostServiceClient
	anon   roomcityv1connect.HostServiceClient
}

func newHostFixture(t *testing.T) *hostFixture {
	t.Helper()
	ctx := context.Background()
	reg, err := session.Open(ctx, filepath.Join(t.TempDir(), "r.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = reg.Close() })
	token, err := reg.Start(ctx, "s", "host", session.AlwaysOn)
	require.NoError(t, err)

	s := server.New()
	s.Handle(roomcityv1connect.NewHostServiceHandler(&server.Host{Registry: reg}))
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return &hostFixture{
		reg:    reg,
		token:  token,
		client: roomcityv1connect.NewHostServiceClient(ts.Client(), ts.URL, sessionauth.Client(token)),
		anon:   roomcityv1connect.NewHostServiceClient(ts.Client(), ts.URL),
	}
}

func TestHostRefusesACallWithoutTheSessionsToken(t *testing.T) {
	f := newHostFixture(t)
	ctx := context.Background()

	_, err := f.anon.Report(ctx, connect.NewRequest(&roomcityv1.ReportRequest{Session: "s"}))
	require.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err))

	_, err = f.client.Report(ctx, connect.NewRequest(&roomcityv1.ReportRequest{Session: "another"}))
	require.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err))

	stream, err := f.anon.Attach(ctx, connect.NewRequest(&roomcityv1.AttachRequest{Session: "s"}))
	require.NoError(t, err)
	require.False(t, stream.Receive())
	require.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(stream.Err()))
}

func TestHostAttachesFromTheLastCarriedInstruction(t *testing.T) {
	f := newHostFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := f.reg.Nudge(ctx, "s", "from gc")
	require.NoError(t, err)
	_, err = f.reg.Prompt(ctx, "s", session.PromptInput{Text: "from owner"})
	require.NoError(t, err)

	stream, err := f.client.Attach(ctx, connect.NewRequest(&roomcityv1.AttachRequest{Session: "s", LastSeq: 1}))
	require.NoError(t, err)
	require.True(t, stream.Receive())
	got := stream.Msg()
	require.Equal(t, uint64(2), got.Seq)
	require.Equal(t, "from owner", got.GetPrompt().Text)
	require.Equal(t, roomcityv1.PromptSource_PROMPT_SOURCE_OWNER, got.GetPrompt().Source)

	require.NoError(t, f.reg.AnswerPermission(ctx, "s", "7", "allow"))
	require.True(t, stream.Receive())
	require.Equal(t, "allow", stream.Msg().GetPermissionAnswer().OptionId)
}

func TestHostReportStoresFramesOnceAndAnswersTheLastNumber(t *testing.T) {
	f := newHostFixture(t)
	ctx := context.Background()
	at := time.Unix(1700000000, 0)
	frames := []*roomcityv1.Frame{
		{Seq: 1, Time: timestamppb.New(at), Direction: roomcityv1.FrameDirection_FRAME_DIRECTION_TO_AGENT, Message: []byte(`{"id":0,"method":"initialize"}`)},
		{Seq: 2, Direction: roomcityv1.FrameDirection_FRAME_DIRECTION_FROM_AGENT, Message: []byte(`{"id":0,"result":{}}`)},
	}
	for range 2 {
		res, err := f.client.Report(ctx, connect.NewRequest(&roomcityv1.ReportRequest{Session: "s", Frames: frames}))
		require.NoError(t, err)
		require.Equal(t, uint64(2), res.Msg.LastSeq)
	}

	evs, err := f.reg.Subscribe(ctx, "s", 1)
	require.NoError(t, err)
	first := (<-evs).(session.Frame)
	require.Equal(t, session.ToAgent, first.Direction)
	require.True(t, first.Time.Equal(at))

	_, err = f.client.Report(ctx, connect.NewRequest(&roomcityv1.ReportRequest{Session: "s", Frames: []*roomcityv1.Frame{{Seq: 9}}}))
	require.Equal(t, connect.CodeOutOfRange, connect.CodeOf(err))
}

func TestHostStageSendsTheBundleInPieces(t *testing.T) {
	f := newHostFixture(t)
	ctx := context.Background()

	// A start that kept no bundle gets an empty manifest.
	stream, err := f.client.Stage(ctx, connect.NewRequest(&roomcityv1.StageRequest{Session: "s"}))
	require.NoError(t, err)
	require.True(t, stream.Receive())
	require.NotNil(t, stream.Msg().GetManifest())
	require.False(t, stream.Receive())
	require.NoError(t, stream.Err())

	manifest, err := proto.Marshal(&roomcityv1.StageManifest{Command: []string{"claude", "--model", "x"}, PreStart: []string{"true"}})
	require.NoError(t, err)
	archive := bytes.Repeat([]byte("tar"), 50_000)
	require.NoError(t, f.reg.PutBundle(ctx, "s", manifest, archive))

	stream, err = f.client.Stage(ctx, connect.NewRequest(&roomcityv1.StageRequest{Session: "s"}))
	require.NoError(t, err)
	require.True(t, stream.Receive())
	require.Equal(t, []string{"claude", "--model", "x"}, stream.Msg().GetManifest().Command)
	var got []byte
	pieces := 0
	for stream.Receive() {
		got = append(got, stream.Msg().GetArchive()...)
		pieces++
	}
	require.NoError(t, stream.Err())
	require.Equal(t, archive, got)
	require.Greater(t, pieces, 1)
}

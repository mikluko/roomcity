package runtimecmd_test

import (
	"bytes"
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	roomcityv1 "sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1"
	"sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1/roomcityv1connect"
	"sarak.sh/mikluko/roomcity/internal/runtimecmd"
)

type fake struct {
	got *roomcityv1.OperateRequest
	res *roomcityv1.OperateResponse
}

func (f *fake) Operate(_ context.Context, req *connect.Request[roomcityv1.OperateRequest]) (*connect.Response[roomcityv1.OperateResponse], error) {
	f.got = req.Msg
	return connect.NewResponse(f.res), nil
}

func run(t *testing.T, env []string, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = runtimecmd.Run(context.Background(), args, env, runtimecmd.Streams{Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errb})
	return code, out.String(), errb.String()
}

func serve(t *testing.T, f *fake) string {
	t.Helper()
	_, h := roomcityv1connect.NewRuntimeServiceHandler(f)
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts.URL
}

func TestTheOperationReachesTheServerWithItsArgumentsStdinAndGcVariables(t *testing.T) {
	f := &fake{res: &roomcityv1.OperateResponse{Stdout: []byte("true"), Stderr: []byte("note\n"), ExitCode: 0}}
	url := serve(t, f)
	env := []string{runtimecmd.EnvURL + "=" + url, "GC_CITY=/city", "HOME=/root", "GC_EMPTY="}

	code, stdout, stderr := run(t, env, "payload", "set-meta", "s-1", "key")
	require.Zero(t, code)
	require.Equal(t, "true", stdout)
	require.Equal(t, "note\n", stderr)
	require.Equal(t, "set-meta", f.got.Operation)
	require.Equal(t, []string{"s-1", "key"}, f.got.Args)
	require.Equal(t, "payload", string(f.got.Stdin))
	require.Equal(t, map[string]string{"GC_CITY": "/city", "GC_EMPTY": ""}, f.got.Env)
}

func TestTheServersExitCodeIsTheCommands(t *testing.T) {
	for _, want := range []int32{0, 1, 2} {
		f := &fake{res: &roomcityv1.OperateResponse{ExitCode: want}}
		url := serve(t, f)
		code, _, _ := run(t, []string{runtimecmd.EnvURL + "=" + url}, "", "protocol")
		require.Equal(t, int(want), code)
	}
}

func TestAServerThatCannotBeReachedIsAFailureNeverTheCodeGcReadsAsUnknown(t *testing.T) {
	ts := httptest.NewServer(nil)
	url := ts.URL
	ts.Close()

	code, _, stderr := run(t, []string{runtimecmd.EnvURL + "=" + url}, "", "is-running", "s-1")
	require.Equal(t, 1, code)
	require.Contains(t, stderr, "is-running")

	code, _, stderr = run(t, nil, "", "is-running", "s-1")
	require.Equal(t, 1, code)
	require.Contains(t, stderr, runtimecmd.EnvURL)

	code, _, _ = run(t, []string{runtimecmd.EnvURL + "=" + url}, "")
	require.Equal(t, 1, code, "no operation")
}

package host

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
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

// roomSession serves the room over dir and returns an MCP client session of
// it and the requests the server was handed.
func roomSession(t *testing.T, dir string, answer error) (*mcp.ClientSession, *[]*roomcityv1.SendRequest) {
	t.Helper()
	var sent []*roomcityv1.SendRequest
	r, err := startRoom(dir, func(_ context.Context, req *roomcityv1.SendRequest) (string, error) {
		sent = append(sent, req)
		return "$event", answer
	})
	require.NoError(t, err)
	t.Cleanup(r.stop)
	_, address, header := r.entry()
	client := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "1"}, nil)
	cs, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: address, HTTPClient: &http.Client{Transport: bearer{header}}, DisableStandaloneSSE: true,
	}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs.Close() })
	return cs, &sent
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
	r, err := startRoom(t.TempDir(), nil)
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

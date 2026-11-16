package serve_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/id"

	"sarak.sh/mikluko/roomcity/internal/config"
	roomcityv1 "sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1"
	"sarak.sh/mikluko/roomcity/internal/operate"
	"sarak.sh/mikluko/roomcity/internal/runtimecmd"
	"sarak.sh/mikluko/roomcity/internal/serve"
)

// listenOn listens where the homeserver's registration says the city's
// transactions are sent.
func listenOn(t *testing.T, addr string) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", addr)
	require.NoError(t, err)
	return l
}

type forwardedFor struct{ next http.RoundTripper }

func (f forwardedFor) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("X-Forwarded-For", "127.0.0.1")
	return f.next.RoundTrip(req)
}

// chatConfig is a roomcity.toml with a city on the local Tuwunel, the tokens in
// files as the Secret of the namespace gives them.
func chatConfig(t *testing.T, hs, netURL string, holder string) config.Config {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "as_token"), []byte("testserve-as-token\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hs_token"), []byte("testserve-hs-token\n"), 0o600))
	cfg, err := config.Parse([]byte(`
[server]
data = "` + filepath.Join(t.TempDir(), "data") + `"
city_dir = "` + t.TempDir() + `"
url = "` + netURL + `"

[agents.host]
always_on = true

[kube]
namespace = "sandbox"
image = "roomcity"

[chat]
name = "testserve"
holders = [{ id = "` + holder + `", role = "owner" }]

[matrix]
homeserver = "` + hs + `"
server_name = "test.local"
city = "testserve"
as_token_file = "` + filepath.Join(dir, "as_token") + `"
hs_token_file = "` + filepath.Join(dir, "hs_token") + `"
`))
	require.NoError(t, err)
	return cfg
}

func TestServeWholeMakesTheRoomsOfTheCityOnTheHomeserver(t *testing.T) {
	hs := os.Getenv("ROOMCITY_TEST_HOMESERVER")
	if hs == "" && os.Getenv("ROOMCITY_TEST_REQUIRE_HOMESERVER") != "" {
		t.Fatal("ROOMCITY_TEST_REQUIRE_HOMESERVER is set, so the homeserver is not optional")
	}
	if hs == "" {
		t.Skip("ROOMCITY_TEST_HOMESERVER is not set: run scripts/tuwunel-test.sh")
	}
	// The city "testserve" of the script has its own port, apart from the
	// driver's tests', which run beside this one.
	listenAt := os.Getenv("ROOMCITY_TEST_SERVE_LISTEN")
	if listenAt == "" {
		listenAt = "0.0.0.0:29394"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// The owner is an application user of the homeserver, as the tests' registration has it.
	reg := appservice.CreateRegistration()
	reg.ID, reg.SenderLocalpart = "testowner", "_testowner"
	reg.AppToken, reg.ServerToken = "testowner-as-token", "testowner-hs-token"
	owners, err := appservice.CreateFull(appservice.CreateOpts{Registration: reg, HomeserverDomain: "test.local", HomeserverURL: hs})
	require.NoError(t, err)
	owners.HTTPClient.Transport = forwardedFor{http.DefaultTransport}
	alice := owners.NewIntentAPI(fmt.Sprintf("_testowner_serve%d", time.Now().UnixNano()))
	require.NoError(t, alice.EnsureRegistered(ctx))

	ls := serve.Listeners{Network: listenOn(t, listenAt), Runtime: listen(t), Page: listen(t)}
	netURL := "http://" + ls.Network.Addr().String()
	runtimeURL := "http://" + ls.Runtime.Addr().String()
	cfg := chatConfig(t, hs, netURL, string(alice.UserID))

	prompts := make(chan *roomcityv1.AttachResponse, 8)
	staged := make(chan *roomcityv1.StageManifest, 1)
	j := &jobs{alive: map[string]bool{}}
	j.fn = func(spec operate.JobSpec) { pod(t, ctx, spec, prompts, staged) }

	served := make(chan error, 1)
	runCtx, stop := context.WithCancel(ctx)
	go func() { served <- serve.Run(runCtx, serve.Options{Config: cfg, Listeners: ls, Jobs: j}) }()

	var out, errb bytes.Buffer
	startJSON, _ := json.Marshal(map[string]any{
		"work_dir": t.TempDir(), "command": "claude", "env": map[string]string{"GC_AGENT": "host"}, "nudge": "begin",
	})
	code := runtimecmd.Run(ctx, []string{"start", "host-1"}, []string{runtimecmd.EnvURL + "=" + runtimeURL, "GC_CITY=/city"},
		runtimecmd.Streams{Stdin: strings.NewReader(string(startJSON)), Stdout: &out, Stderr: &errb})
	require.Zero(t, code, errb.String())

	var space, journal string
	require.Eventually(t, func() bool {
		r, err := alice.JoinRoom(ctx, "#testserve:test.local", nil)
		if err != nil {
			return false
		}
		space = r.RoomID.String()
		r, err = alice.JoinRoom(ctx, "#testserve-journal:test.local", nil)
		if err != nil {
			return false
		}
		journal = r.RoomID.String()
		return true
	}, 30*time.Second, 200*time.Millisecond, "the space and the journal of the city are made, and the holder is invited")
	require.NotEmpty(t, space)
	require.NotEmpty(t, journal)
	var host id.RoomID
	require.Eventually(t, func() bool {
		r, err := alice.JoinRoom(ctx, "#testserve-host:test.local", nil)
		if err != nil {
			return false
		}
		host = r.RoomID
		return true
	}, 30*time.Second, 200*time.Millisecond, "the room of the always-on agent is made as its session starts")

	// The homeserver's transactions reach the server: what the owner writes in
	// the room is a prompt of the session, after the nudge that began it.
	_, err = alice.SendText(ctx, host, "hello from the room")
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		select {
		case in := <-prompts:
			return in.GetPrompt().GetText() == "hello from the room"
		default:
			return false
		}
	}, 30*time.Second, 100*time.Millisecond, "the owner's message reaches the session")

	stop()
	require.NoError(t, <-served)
}

func TestServeRefusesAChatWithoutItsMatrixAndATokenItCannotRead(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	run := func(extra string) error {
		ls := serve.Listeners{Network: listen(t), Runtime: listen(t), Page: listen(t)}
		defer ls.Close()
		cfg, err := config.Parse([]byte(`
[server]
data = "` + filepath.Join(t.TempDir(), "data") + `"
city_dir = "` + t.TempDir() + `"
url = "http://x"

[kube]
namespace = "sandbox"
image = "roomcity"
` + extra))
		require.NoError(t, err)
		return serve.Run(ctx, serve.Options{Config: cfg, Listeners: ls, Jobs: &jobs{alive: map[string]bool{}}})
	}

	err := run(`
[chat]
holders = [{ id = "@a:sarak.sh", role = "owner" }]
`)
	require.ErrorContains(t, err, "[matrix] is not")

	err = run(`
[matrix]
homeserver = "http://tuwunel:8008"
server_name = "sarak.sh"
city = "sandbox"
as_token_file = "/nonexistent/as"
hs_token_file = "/nonexistent/hs"
`)
	require.ErrorContains(t, err, "reading a token")
}

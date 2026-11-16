package transcripts_test

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"sarak.sh/mikluko/roomcity/internal/session"
	"sarak.sh/mikluko/roomcity/internal/transcripts"
)

func said(text string) session.HostFrame {
	return session.HostFrame{Seq: 1, Direction: session.FromAgent, Message: []byte(
		`{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"` + text + `"}}}}`)}
}

// twoStarts is a session that said "first run" and was reset, and then said
// "second run" and is still running.
func twoStarts(t *testing.T) *session.Registry {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	reg, err := session.Open(ctx, filepath.Join(t.TempDir(), "r.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = reg.Close() })
	reg.SetResetter(func(context.Context, string) error { return nil })

	_, err = reg.StartWith(ctx, "s-host", "host", session.AlwaysOn, session.StartInfo{GCID: "gc-1"})
	require.NoError(t, err)
	require.NoError(t, reg.SetRoom(ctx, "s-host", "!room:test"))
	_, err = reg.Report(ctx, "s-host", []session.HostFrame{said("first run")})
	require.NoError(t, err)
	require.NoError(t, reg.Reset(ctx, "s-host"))
	require.NoError(t, reg.End(ctx, "s-host"))

	_, err = reg.StartWith(ctx, "s-host", "host", session.AlwaysOn, session.StartInfo{GCID: "gc-2"})
	require.NoError(t, err)
	_, err = reg.Report(ctx, "s-host", []session.HostFrame{said("second run")})
	require.NoError(t, err)
	return reg
}

func TestPageIsOfOneStartAndTheLatestWithoutANumber(t *testing.T) {
	h := (&transcripts.Page{Source: twoStarts(t), City: "sandbox"}).Handler()

	code, body, _ := get(t, h, "/-/s-host")
	require.Equal(t, http.StatusOK, code)
	require.Contains(t, body, "second run")
	require.NotContains(t, body, "first run", "nothing of the earlier start")
	require.Contains(t, body, "2 of 2")

	code, body, _ = get(t, h, "/-/s-host/1")
	require.Equal(t, http.StatusOK, code)
	require.Contains(t, body, "first run")
	require.NotContains(t, body, "second run")
	require.Contains(t, body, `href="/-/s-host/2"`, "the link to the other start")
}

func TestCardSaysHowTheStartEndedItsRoomAndGcsSession(t *testing.T) {
	h := (&transcripts.Page{Source: twoStarts(t), City: "sandbox"}).Handler()

	_, closed, _ := get(t, h, "/-/s-host/1")
	for _, want := range []string{">reset<", "!room:test", "gc-1"} {
		require.Contains(t, closed, want)
	}
	require.Contains(t, closed, "ended")

	_, open, _ := get(t, h, "/-/s-host/2")
	require.Contains(t, open, "gc-2")
	require.Contains(t, open, "not yet", "an open start has not ended")
	require.Contains(t, open, "none recorded", "and no room has been recorded for it")
}

func TestRawIsOfOneStart(t *testing.T) {
	h := (&transcripts.Page{Source: twoStarts(t), City: "sandbox"}).Handler()

	code, body, header := get(t, h, "/-/s-host/1/raw")
	require.Equal(t, http.StatusOK, code)
	require.Contains(t, body, "first run")
	require.NotContains(t, body, "second run")
	require.Contains(t, header.Get("Content-Disposition"), "s-host-1.jsonl")
	require.Equal(t, 3, len(strings.Split(strings.TrimSpace(body), "\n")), "started, one frame, stopped")
}

func TestPageSaysSoForAStartThatIsNotThere(t *testing.T) {
	h := (&transcripts.Page{Source: twoStarts(t), City: "sandbox"}).Handler()
	for _, path := range []string{"/-/s-host/3", "/-/s-host/0", "/-/s-host/x", "/-/s-host/3/raw", "/-/nobody/1"} {
		code, _, _ := get(t, h, path)
		require.Equal(t, http.StatusNotFound, code, path)
	}
}

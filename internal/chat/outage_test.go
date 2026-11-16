package chat

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOutageLogsTheFirstFailureOnceAMinuteAfterAndTheRecovery(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	lines := func() []string {
		out := strings.Split(strings.TrimSpace(buf.String()), "\n")
		buf.Reset()
		if len(out) == 1 && out[0] == "" {
			return nil
		}
		return out
	}
	failed := attempt{again: true, err: errors.New("down")}
	t0 := time.Unix(1000, 0)
	var o outage

	o.note(log, "s", "host", failed, t0)
	require.Len(t, lines(), 1, "the first failure")
	for i := 1; i <= 11; i++ {
		o.note(log, "s", "host", failed, t0.Add(time.Duration(i)*5*time.Second))
	}
	require.Empty(t, lines(), "every five seconds for a minute says nothing more")

	o.note(log, "s", "host", failed, t0.Add(61*time.Second))
	got := lines()
	require.Len(t, got, 1, "a line a minute after the first")
	require.Contains(t, got[0], "still does not take")

	o.note(log, "s", "host", attempt{again: true, sent: 2}, t0.Add(90*time.Second))
	got = lines()
	require.Len(t, got, 1)
	require.Contains(t, got[0], "takes what is sent to it again")

	o.note(log, "s", "host", failed, t0.Add(100*time.Second))
	require.Len(t, lines(), 1, "an outage after the recovery is a first failure again")
}

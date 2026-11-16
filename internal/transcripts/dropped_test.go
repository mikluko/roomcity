package transcripts_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"sarak.sh/mikluko/roomcity/internal/session"
	"sarak.sh/mikluko/roomcity/internal/transcripts"
)

type fixed struct{ entries []session.Entry }

func (fixed) List(context.Context) ([]session.Info, error) { return nil, nil }

func (f fixed) Transcript(context.Context, string, int) (session.Card, []session.Entry, error) {
	return session.Card{Info: session.Info{Name: "s"}}, f.entries, nil
}

func TestRawNamesEveryStateAPromptReaches(t *testing.T) {
	at := time.Unix(1000, 0)
	var entries []session.Entry
	for i, state := range []session.PromptState{session.Stored, session.Waiting, session.Delivered, session.Dropped} {
		entries = append(entries, session.Entry{Time: at, Event: session.Prompt{Seq: uint64(i + 1), Source: session.FromOwner, Text: "x", State: state, Of: 1}})
	}
	_, body, _ := get(t, (&transcripts.Page{Source: fixed{entries}, City: "sandbox"}).Handler(), "/-/s/raw")

	var states []string
	for _, l := range strings.Split(strings.TrimSpace(body), "\n") {
		var line struct {
			State string `json:"state"`
		}
		require.NoError(t, json.Unmarshal([]byte(l), &line))
		states = append(states, line.State)
	}
	require.Equal(t, []string{"stored", "waiting", "delivered", "dropped"}, states)
}

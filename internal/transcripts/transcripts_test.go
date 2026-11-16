package transcripts_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"sarak.sh/mikluko/roomcity/internal/session"
	"sarak.sh/mikluko/roomcity/internal/transcripts"
)

func get(t *testing.T, h http.Handler, path string) (int, string, http.Header) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code, rec.Body.String(), rec.Header()
}

// finished is a session of worker that has run a turn and ended, with an
// output longer than a room shows and an agent that writes markup.
func finished(t *testing.T) (*session.Registry, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	reg, err := session.Open(ctx, filepath.Join(t.TempDir(), "r.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = reg.Close() })
	const name = "s-worker 1"
	_, err = reg.Start(ctx, name, "worker", session.Task)
	require.NoError(t, err)
	_, err = reg.Prompt(ctx, name, session.PromptInput{Text: "count the words"})
	require.NoError(t, err)
	long := strings.Repeat("o", 9000)
	frames := []string{
		`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"tool_call","toolCallId":"t","title":"Run it","status":"pending","rawInput":{"command":"wc"}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"tool_call_update","toolCallId":"t","status":"completed","content":[{"type":"content","content":{"type":"text","text":"` + long + `"}}]}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Nine <script>alert(1)</script> [x](javascript:alert(1))"}}}}`,
		`{"jsonrpc":"2.0","id":1,"result":{"stopReason":"end_turn"}}`,
	}
	var hf []session.HostFrame
	for i, f := range frames {
		d := session.FromAgent
		if i == 0 {
			d = session.ToAgent
		}
		hf = append(hf, session.HostFrame{Seq: uint64(i + 1), Direction: d, Message: []byte(f)})
	}
	_, err = reg.Report(ctx, name, hf)
	require.NoError(t, err)
	require.NoError(t, reg.End(ctx, name))
	return reg, name
}

func TestPageGivesAFinishedSessionWhole(t *testing.T) {
	reg, name := finished(t)
	h := (&transcripts.Page{Source: reg, City: "sandbox"}).Handler()

	code, body, header := get(t, h, "/-/s-worker%201")
	require.Equal(t, http.StatusOK, code)
	require.Contains(t, header.Get("Content-Type"), "text/html")
	require.Equal(t, "no-store", header.Get("Cache-Control"))
	require.Contains(t, body, "sandbox")
	require.Contains(t, body, name)
	require.Contains(t, body, "worker")
	require.Contains(t, body, "ended")
	require.Contains(t, body, "count the words", "the owner's own words")
	require.Contains(t, body, "Run it", "the tool call")
	require.Contains(t, body, strings.Repeat("o", 9000), "the output a room would cut at 4000 bytes, whole")
	require.Contains(t, body, "/-/s-worker%201/raw", "the link to the raw stream")
}

func TestPageEscapesWhatTheAgentWrote(t *testing.T) {
	reg, _ := finished(t)
	_, body, _ := get(t, (&transcripts.Page{Source: reg, City: "sandbox"}).Handler(), "/-/s-worker%201")
	require.NotContains(t, body, "<script>alert(1)</script>")
	require.Contains(t, body, "raw HTML omitted", "markup in the text is dropped, not run")
	require.NotContains(t, body, "javascript:", "a link to a script is dropped")
}

func TestRawIsTheStreamAsItWasOneJSONMessageALine(t *testing.T) {
	reg, _ := finished(t)
	code, body, header := get(t, (&transcripts.Page{Source: reg, City: "sandbox"}).Handler(), "/-/s-worker%201/raw")
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "application/x-ndjson", header.Get("Content-Type"))
	require.Contains(t, header.Get("Content-Disposition"), "attachment")

	var lines []map[string]any
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 0, 1<<20), 1<<24)
	for sc.Scan() {
		var l map[string]any
		require.NoError(t, json.Unmarshal(sc.Bytes(), &l))
		lines = append(lines, l)
	}
	require.NoError(t, sc.Err())
	var types []string
	for i, l := range lines {
		require.EqualValues(t, i+1, l["seq"], "in order, from 1")
		types = append(types, l["type"].(string))
	}
	// The prompt was never delivered: the end closes its record.
	require.Equal(t, []string{"lifecycle", "prompt", "frame", "frame", "frame", "frame", "frame", "prompt", "lifecycle"}, types)
	require.Equal(t, "to_agent", lines[2]["direction"])
	require.Equal(t, map[string]any{"jsonrpc": "2.0", "id": float64(1), "result": map[string]any{"stopReason": "end_turn"}}, lines[6]["message"])
	require.Equal(t, "stopped", lines[8]["kind"])
}

func TestPageSaysSoForASessionThatIsNotThere(t *testing.T) {
	reg, _ := finished(t)
	h := (&transcripts.Page{Source: reg, City: "sandbox"}).Handler()
	for _, path := range []string{"/-/nobody", "/-/nobody/raw"} {
		code, _, _ := get(t, h, path)
		require.Equal(t, http.StatusNotFound, code, path)
	}
}

func TestIndexListsTheSessionsWithTheirLinks(t *testing.T) {
	reg, _ := finished(t)
	code, body, _ := get(t, (&transcripts.Page{Source: reg, City: "sandbox"}).Handler(), "/-/")
	require.Equal(t, http.StatusOK, code)
	require.Contains(t, body, `href="/-/s-worker%201"`)
}

func TestPageIsReadOnly(t *testing.T) {
	reg, _ := finished(t)
	h := (&transcripts.Page{Source: reg, City: "sandbox"}).Handler()
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, "/-/s-worker%201", nil))
		require.Equal(t, http.StatusMethodNotAllowed, rec.Code, method)
	}
}

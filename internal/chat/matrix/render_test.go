package matrix

import (
	"testing"

	"github.com/stretchr/testify/require"

	"sarak.sh/mikluko/roomcity/internal/chat"
)

func TestRenderKeepsATablesRowsInThePlainBody(t *testing.T) {
	table := "| Country | Capital |\n|---|---|\n| Belarus | Minsk |\n| France | Paris |"
	content := render(chat.Message{Kind: chat.Text, Body: "Capitals:\n\n" + table})

	require.Contains(t, content.FormattedBody, "<table>")
	require.Contains(t, content.Body, "| Belarus | Minsk |\n| France | Paris |", "rows and separators, not cells run together")
}

func TestRenderOfAFoldWithNoWordsOfItsOwnHasNoEmptyLineFirst(t *testing.T) {
	content := render(chat.Message{Kind: chat.Notice, Fold: &chat.Fold{Summary: "💭 Thinking", Text: "Mars."}})

	require.Equal(t, "💭 Thinking\nMars.", content.Body, "what a client that shows the plain text shows first")
	require.Equal(t, "<details><summary>💭 Thinking</summary><blockquote>Mars.</blockquote></details>", content.FormattedBody,
		"and the HTML starts with the fold, not with an empty paragraph")
}

func TestRenderOfAFoldAfterWordsKeepsTheWordsFirst(t *testing.T) {
	content := render(chat.Message{Kind: chat.Notice, Body: "✅ done", Fold: &chat.Fold{Summary: "output", Text: "ok", Code: true}})

	require.Equal(t, "✅ done\nok", content.Body)
	require.Equal(t, "<p>✅ done</p><details><summary>output</summary><pre><code>ok</code></pre></details>", content.FormattedBody)
}

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

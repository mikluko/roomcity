package transcripts

import (
	"testing"

	"github.com/stretchr/testify/require"

	"sarak.sh/mikluko/roomcity/internal/session"
)

func held(ns ...int) []session.Start {
	var out []session.Start
	for _, n := range ns {
		out = append(out, session.Start{N: n})
	}
	return out
}

func TestNavigationOfAStartWhoseNeighboursWereRemoved(t *testing.T) {
	// The 97 starts of a session, of which the term has left 5, 95, 96 and 97.
	card := session.Card{Start: session.Start{N: 96}, Starts: held(5, 95, 96, 97)}

	require.Equal(t, navigation{First: 5, Prev: 95, Next: 97, Latest: 97, Total: 97}, navigate(card),
		"of 97, not of the 4 that are held; the ends and the neighbours, not a link to each")
}

func TestNavigationAtTheEnds(t *testing.T) {
	require.Equal(t, navigation{Next: 2, Latest: 3, Total: 3}, navigate(session.Card{Start: session.Start{N: 1}, Starts: held(1, 2, 3)}))
	require.Equal(t, navigation{First: 1, Prev: 2, Total: 3}, navigate(session.Card{Start: session.Start{N: 3}, Starts: held(1, 2, 3)}))
	require.Equal(t, navigation{Total: 1}, navigate(session.Card{Start: session.Start{N: 1}, Starts: held(1)}))
}

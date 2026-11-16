package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"sarak.sh/mikluko/roomcity/internal/config"
)

func TestTranscriptTermIsNinetyDaysUnlessTheFileSaysOtherwise(t *testing.T) {
	c, err := config.Parse([]byte(minimal))
	require.NoError(t, err)
	require.Equal(t, 90*24*time.Hour, c.Server.TranscriptTerm.Duration())

	c, err = config.Parse([]byte(strings.Replace(minimal, "[kube]", "transcript_term = \"720h\"\n\n[kube]", 1)))
	require.NoError(t, err)
	require.Equal(t, 720*time.Hour, c.Server.TranscriptTerm.Duration())

	_, err = config.Parse([]byte(strings.Replace(minimal, "[kube]", "transcript_term = \"-1h\"\n\n[kube]", 1)))
	require.ErrorContains(t, err, "negative")
}

func TestPurgeIsEveryHourUnlessTheFileSaysOtherwise(t *testing.T) {
	c, err := config.Parse([]byte(minimal))
	require.NoError(t, err)
	require.Equal(t, time.Hour, c.Server.PurgeEvery.Duration())

	c, err = config.Parse([]byte(strings.Replace(minimal, "[kube]", "transcript_term = \"20m\"\npurge_every = \"1m\"\n\n[kube]", 1)))
	require.NoError(t, err)
	require.Equal(t, time.Minute, c.Server.PurgeEvery.Duration(), "a term and a gap short enough to watch a transcript go")

	_, err = config.Parse([]byte(strings.Replace(minimal, "[kube]", "purge_every = \"-1m\"\n\n[kube]", 1)))
	require.ErrorContains(t, err, "negative")
}

package host

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeSecret(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func TestAgentEnvSetsEachVariableTheServerNamesFromItsFile(t *testing.T) {
	a, b := writeSecret(t, "from-file\n"), writeSecret(t, "other")

	got, err := agentEnv([]string{"PATH=/bin", "A_TOKEN=old", SecretPrefix + "A_TOKEN=" + a, SecretPrefix + "B_KEY=" + b})

	require.NoError(t, err)
	require.ElementsMatch(t, []string{"PATH=/bin", SecretPrefix + "A_TOKEN=" + a, SecretPrefix + "B_KEY=" + b, "A_TOKEN=from-file", "B_KEY=other"}, got)
}

func TestAgentEnvRefusesAFileItCannotReadAndSaysWhich(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	_, err := agentEnv([]string{SecretPrefix + "A_TOKEN=" + missing})
	require.ErrorContains(t, err, "A_TOKEN")
	require.ErrorContains(t, err, missing)

	empty := writeSecret(t, " \n")
	_, err = agentEnv([]string{SecretPrefix + "A_TOKEN=" + empty})
	require.ErrorContains(t, err, "is empty")
}

func TestAProcessAgentGetsTheSecretFromItsFile(t *testing.T) {
	t.Setenv(SecretPrefix+"KIMI_CODE_TOKEN", writeSecret(t, "kimi-key\n"))
	h := newHost(Config{WorkDir: t.TempDir()})

	ag, err := h.startProcess(context.Background(), []string{"sh", "-c", `printf %s "$KIMI_CODE_TOKEN"`})
	require.NoError(t, err)
	got, err := io.ReadAll(ag.r)
	require.NoError(t, err)

	require.Equal(t, "kimi-key", string(got))
	<-ag.ended
}

func TestAProcessAgentIsNotStartedWhenItsSecretFileIsMissing(t *testing.T) {
	t.Setenv(SecretPrefix+"KIMI_CODE_TOKEN", filepath.Join(t.TempDir(), "absent"))
	h := newHost(Config{WorkDir: t.TempDir()})

	_, err := h.startProcess(context.Background(), []string{"sh", "-c", "true"})

	require.ErrorContains(t, err, "KIMI_CODE_TOKEN")
}

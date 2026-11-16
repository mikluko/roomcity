package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"sarak.sh/mikluko/roomcity/internal/config"
)

const minimal = `
[server]
data = "/var/lib/roomcity"
city_dir = "/city"
url = "http://roomcity.sandbox.svc:8080"

[kube]
namespace = "sandbox"
image = "sarak.sh/mikluko/roomcity:0.2.0"
`

func TestMinimalFileTakesTheDefaults(t *testing.T) {
	c, err := config.Parse([]byte(minimal))
	require.NoError(t, err)
	require.Equal(t, "gc", c.Server.Gc)
	require.Equal(t, ":8080", c.Server.Listen)
	require.Equal(t, "127.0.0.1:8082", c.Server.RuntimeListen)
	require.Equal(t, "127.0.0.1:8081", c.Server.PageListen)
	require.Nil(t, c.Matrix)
	require.False(t, c.AlwaysOn("host"))
}

func TestFullFile(t *testing.T) {
	c, err := config.Parse([]byte(strings.Replace(minimal, "[kube]", `
start_timeout = "90s"
stop_grace = "3s"

[agents.host]
always_on = true

[agents.worker]

[chat]
holders = [{ id = "@mikluko:sarak.sh", role = "owner" }]

[matrix]
homeserver = "http://tuwunel:8008"
server_name = "sarak.sh"
city = "sandbox"
as_token_file = "/run/secrets/as"
hs_token_file = "/run/secrets/hs"

[kube]
run_as_user = 1000
secrets = [{ name = "dolt", key = "credentials", mount_path = "/run/roomcity/secrets/dolt/credentials" }]
`, 1)))
	require.NoError(t, err)
	require.Equal(t, 90*time.Second, c.Server.StartTimeout.Duration())
	require.True(t, c.AlwaysOn("host"))
	require.False(t, c.AlwaysOn("worker"))
	require.Equal(t, "dolt", c.Kube.Secrets[0].Name)
	require.Equal(t, "sandbox", c.Matrix.City)
}

func TestAnAgentNamesItsOwnSecretsAndTheNamespaceTheNodes(t *testing.T) {
	c, err := config.Parse([]byte(strings.Replace(minimal, "[kube]", `
[agents.guest]
secrets = [{ name = "kimi", key = "api-key", mount_path = "/run/kimi/api-key" }]

[kube]
node_selector = { "kubernetes.io/hostname" = "bulgakov" }
tolerations = [{ key = "sarak.sh/workstation", operator = "Exists", effect = "NoSchedule" }]
cpu_request = "250m"
memory_request = "512Mi"
`, 1)))
	require.NoError(t, err)
	require.Equal(t, "kimi", c.AgentSecrets()["guest"][0].Name)
	require.NotContains(t, c.AgentSecrets(), "host")
	require.Equal(t, "bulgakov", c.Kube.NodeSelector["kubernetes.io/hostname"])
	require.Equal(t, "Exists", c.Kube.Tolerations[0].Operator)

	_, err = config.Parse([]byte(minimal + "cpu_request = \"lots\"\n"))
	require.ErrorContains(t, err, "quantities")
	_, err = config.Parse([]byte(strings.Replace(minimal, "[kube]", "[agents.guest]\nsecrets = [{ name = \"kimi\" }]\n\n[kube]", 1)))
	require.ErrorContains(t, err, "agents.guest.secrets[0]")
}

func TestASecretNamesItsVariableOnlyWithAKey(t *testing.T) {
	with := func(secret string) error {
		_, err := config.Parse([]byte(strings.Replace(minimal, "[kube]", "[agents.guest]\nsecrets = [{ "+secret+" }]\n\n[kube]", 1)))
		return err
	}
	require.NoError(t, with(`name = "kimi", key = "api-key", mount_path = "/k", env = "KIMI_CODE_TOKEN"`))
	require.ErrorContains(t, with(`name = "kimi", mount_path = "/k", env = "KIMI_CODE_TOKEN"`), "needs a key")
	require.ErrorContains(t, with(`name = "kimi", key = "api-key", mount_path = "/k", env = "kimi-token"`), "variable name")

	_, err := config.Parse([]byte(minimal + "secrets = [{ name = \"claude\", key = \"token\", mount_path = \"/c\", env = \"x\" }]\n"))
	require.ErrorContains(t, err, "kube.secrets[0]")
}

func TestAgentsThatAreServicesRunAsProcesses(t *testing.T) {
	c, err := config.Parse([]byte(strings.Replace(minimal, "[kube]", `
[agents."core.*"]
process = true

[agents."bd.dog"]
process = true

[agents.host]
always_on = true

[kube]`, 1)))
	require.NoError(t, err)
	require.True(t, c.IsProcess("core.control-dispatcher"))
	require.True(t, c.IsProcess("bd.dog"))
	require.False(t, c.IsProcess("bd.dog-2"))
	require.False(t, c.IsProcess("host"))
	require.False(t, c.IsProcess(""))

	for _, bad := range []string{
		"[agents.svc]\nprocess = true\nalways_on = true\n",
		"[agents.svc]\nprocess = true\nsecrets = [{ name = \"s\", mount_path = \"/s\" }]\n",
		"[agents.\"[\"]\nprocess = true\n",
	} {
		_, err := config.Parse([]byte(strings.Replace(minimal, "[kube]", bad+"\n[kube]", 1)))
		require.Error(t, err, bad)
	}
}

func TestAnUnknownKeyIsAnError(t *testing.T) {
	_, err := config.Parse([]byte(strings.Replace(minimal, "[kube]", "listn = \":1\"\n\n[kube]", 1)))
	require.ErrorContains(t, err, "listn")
}

func TestRuntimeAndPageListenersAreLoopbackOnly(t *testing.T) {
	for _, key := range []string{"runtime_listen", "page_listen"} {
		for _, addr := range []string{":8082", "0.0.0.0:8082", "10.0.0.1:8082", "localhost:8082"} {
			_, err := config.Parse([]byte(strings.Replace(minimal, "[kube]", key+" = \""+addr+"\"\n\n[kube]", 1)))
			require.ErrorContains(t, err, "loopback", key+" "+addr)
		}
		_, err := config.Parse([]byte(strings.Replace(minimal, "[kube]", key+" = \"[::1]:8082\"\n\n[kube]", 1)))
		require.NoError(t, err)
	}
}

func TestRequiredKeys(t *testing.T) {
	for _, drop := range []string{`data = "/var/lib/roomcity"`, `city_dir = "/city"`, `url = "http://roomcity.sandbox.svc:8080"`, `namespace = "sandbox"`} {
		_, err := config.Parse([]byte(strings.Replace(minimal, drop, "", 1)))
		require.Error(t, err, drop)
	}
	_, err := config.Parse([]byte(strings.Replace(minimal, `"/var/lib/roomcity"`, `"relative"`, 1)))
	require.ErrorContains(t, err, "absolute")
}

func TestChatAndMatrixAreValidatedWithTheOwners(t *testing.T) {
	_, err := config.Parse([]byte(minimal + `
[chat]
holders = [{ id = "mikluko", role = "owner" }]

[matrix]
homeserver = "http://tuwunel:8008"
server_name = "sarak.sh"
city = "sandbox"
as_token_file = "/a"
hs_token_file = "/h"
`))
	require.ErrorContains(t, err, "not a Matrix id")
}

func TestLoadReadsAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "roomcity.toml")
	require.NoError(t, os.WriteFile(path, []byte(minimal), 0o644))
	c, err := config.Load(path)
	require.NoError(t, err)
	require.Equal(t, "/city", c.Server.CityDir)
	_, err = config.Load(filepath.Join(t.TempDir(), "absent.toml"))
	require.Error(t, err)
}

func TestASecretMountedInsideTheHomeIsRefusedAndOneOutsideMayNameItsPathVariable(t *testing.T) {
	_, err := config.Parse([]byte(minimal + "secrets = [{ name = \"dolt\", key = \"credentials\", mount_path = \"/home/city/.config/beads/credentials\" }]\n"))
	require.ErrorContains(t, err, "inside the home")

	c, err := config.Parse([]byte(minimal + "secrets = [{ name = \"dolt\", key = \"credentials\", mount_path = \"/run/roomcity/secrets/dolt/credentials\", path_env = \"BEADS_CREDENTIALS_FILE\" }]\n"))
	require.NoError(t, err)
	require.Equal(t, "BEADS_CREDENTIALS_FILE", c.Kube.Secrets[0].PathEnv)

	_, err = config.Parse([]byte(strings.Replace(minimal, "[kube]", "[agents.guest]\nsecrets = [{ name = \"kimi\", key = \"k\", mount_path = \"/home/city/.kimi/key\" }]\n\n[kube]", 1)))
	require.ErrorContains(t, err, "agents.guest.secrets[0]")
}

package matrix_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pelletier/go-toml/v2"
	"github.com/stretchr/testify/require"

	"sarak.sh/mikluko/roomcity/internal/chat"
	"sarak.sh/mikluko/roomcity/internal/chat/matrix"
)

func TestConfigReadsTheMatrixTableOfRoomcityToml(t *testing.T) {
	var file struct {
		Matrix matrix.Config `toml:"matrix"`
	}
	require.NoError(t, toml.Unmarshal([]byte(`
[matrix]
homeserver = "http://tuwunel.matrix.svc:8008"
server_name = "sarak.sh"
city = "sandbox"
as_token_file = "/run/secrets/matrix/as_token"
hs_token_file = "/run/secrets/matrix/hs_token"
`), &file))
	require.NoError(t, file.Matrix.Validate([]chat.UserID{"@mikluko:sarak.sh"}))
	require.Equal(t, "sandbox", file.Matrix.City)
}

func TestConfigValidate(t *testing.T) {
	valid := matrix.Config{
		Homeserver: "http://hs", ServerName: "sarak.sh", City: "sandbox",
		ASTokenFile: "as", HSTokenFile: "hs",
	}
	tests := []struct {
		name   string
		mutate func(*matrix.Config)
		owners []chat.UserID
		want   string
	}{
		{"valid", func(*matrix.Config) {}, []chat.UserID{"@a:sarak.sh"}, ""},
		{"no homeserver", func(c *matrix.Config) { c.Homeserver = "" }, nil, "homeserver"},
		{"no server name", func(c *matrix.Config) { c.ServerName = "" }, nil, "server_name"},
		{"a city name with a dash", func(c *matrix.Config) { c.City = "my-city" }, nil, "city"},
		{"no token file", func(c *matrix.Config) { c.HSTokenFile = "" }, nil, "token_file"},
		{"an owner that is no Matrix id", func(*matrix.Config) {}, []chat.UserID{"mikluko"}, "not a Matrix id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := valid
			tt.mutate(&c)
			err := c.Validate(tt.owners)
			if tt.want == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestTokensAreReadFromTheirFiles(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "as"), []byte("as-token\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hs"), []byte("hs-token"), 0o600))
	as, hs, err := matrix.Config{ASTokenFile: filepath.Join(dir, "as"), HSTokenFile: filepath.Join(dir, "hs")}.Tokens()
	require.NoError(t, err)
	require.Equal(t, "as-token", as)
	require.Equal(t, "hs-token", hs)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "empty"), []byte("\n"), 0o600))
	_, _, err = matrix.Config{ASTokenFile: filepath.Join(dir, "empty"), HSTokenFile: filepath.Join(dir, "hs")}.Tokens()
	require.ErrorContains(t, err, "empty")
}

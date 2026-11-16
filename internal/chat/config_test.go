package chat

import (
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"
	"github.com/stretchr/testify/require"
)

const sandboxConfig = `
[chat]
name = "sandbox"
transcripts = "https://sandbox.gs.sarak.sh/-/"

[[chat.holders]]
id = "@mikluko:sarak.sh"
role = "owner"

[[chat.holders]]
id = "@_prototype_owner:sarak.sh"
role = "owner"
`

func TestConfigReadsTheChatSectionOfRoomcityToml(t *testing.T) {
	var file struct {
		Chat Config `toml:"chat"`
	}
	require.NoError(t, toml.Unmarshal([]byte(sandboxConfig), &file))
	require.NoError(t, file.Chat.Validate())
	require.Equal(t, "sandbox", file.Chat.Name)
	require.Equal(t, "https://sandbox.gs.sarak.sh/-/", file.Chat.Transcripts)
	require.Equal(t, []UserID{"@mikluko:sarak.sh", "@_prototype_owner:sarak.sh"}, file.Chat.Owners())
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		holders []Holder
		want    string
	}{
		{"none", nil, ""},
		{"an owner", []Holder{{ID: "@a:sarak.sh", Role: RoleOwner}}, ""},
		{"no id", []Holder{{Role: RoleOwner}}, "no id"},
		{"a role that does not exist", []Holder{{ID: "@a:sarak.sh", Role: "reader"}}, "only"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Config{Holders: tt.holders}.Validate()
			if tt.want == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestConfigCatchUp(t *testing.T) {
	var file struct {
		Chat Config `toml:"chat"`
	}
	require.NoError(t, toml.Unmarshal([]byte(sandboxConfig), &file))
	require.Equal(t, DefaultCatchUp, file.Chat.CatchUpWindow(), "none is the default")

	require.NoError(t, toml.Unmarshal([]byte("[chat]\ncatch_up = \"90m\"\n"), &file))
	require.NoError(t, file.Chat.Validate())
	require.Equal(t, 90*time.Minute, file.Chat.CatchUpWindow())

	require.NoError(t, toml.Unmarshal([]byte("[chat]\ncatch_up = \"-1m\"\n"), &file))
	require.ErrorContains(t, file.Chat.Validate(), "catch_up")
}

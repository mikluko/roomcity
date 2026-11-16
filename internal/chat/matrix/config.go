package matrix

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"sarak.sh/mikluko/roomcity/internal/chat"
)

// Config is the table [matrix] of roomcity.toml: where the homeserver is and
// how the city is known to it.
type Config struct {
	// Homeserver is the homeserver's address, such as http://tuwunel:8008.
	Homeserver string `toml:"homeserver"`
	// ServerName is the name that follows the colon in the homeserver's ids.
	ServerName string `toml:"server_name"`
	// City is the city's name. The application service's own user is
	// @<City>:<ServerName> and each agent's user @<City>-<agent>:<ServerName>,
	// so a city's name must not hold a dash.
	City string `toml:"city"`
	// ASTokenFile holds the application service's token, HSTokenFile the
	// homeserver's, which each transaction must carry.
	ASTokenFile string `toml:"as_token_file"`
	HSTokenFile string `toml:"hs_token_file"`
}

var (
	cityName = regexp.MustCompile(`^[a-z0-9]+$`)
	userID   = regexp.MustCompile(`^@[^:\s]+:[^:\s]+$`)
)

// Validate reports the first thing wrong with the configuration, or with the
// ids of the owners, which must be Matrix ids.
func (c Config) Validate(owners []chat.UserID) error {
	switch {
	case c.Homeserver == "":
		return errors.New("matrix: homeserver is empty")
	case c.ServerName == "":
		return errors.New("matrix: server_name is empty")
	case !cityName.MatchString(c.City):
		return fmt.Errorf("matrix: city %q is not made of [a-z0-9]", c.City)
	case c.ASTokenFile == "" || c.HSTokenFile == "":
		return errors.New("matrix: as_token_file and hs_token_file are both required")
	}
	for _, o := range owners {
		if !userID.MatchString(string(o)) {
			return fmt.Errorf("matrix: holder %q is not a Matrix id", o)
		}
	}
	return nil
}

// Tokens reads the application service's token and the homeserver's from
// their files.
func (c Config) Tokens() (as, hs string, err error) {
	if as, err = readToken(c.ASTokenFile); err != nil {
		return "", "", err
	}
	if hs, err = readToken(c.HSTokenFile); err != nil {
		return "", "", err
	}
	return as, hs, nil
}

func readToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("matrix: reading a token: %w", err)
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return "", fmt.Errorf("matrix: the token in %s is empty", path)
	}
	return token, nil
}

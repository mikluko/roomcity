package chat

import (
	"fmt"
	"strings"
	"time"
)

// Duration is a time.Duration written as a string, such as "100s".
type Duration time.Duration

// UnmarshalText reads a duration as time.ParseDuration does.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// Duration returns d as a time.Duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// DefaultCatchUp is the CatchUp of a configuration that has none.
const DefaultCatchUp = time.Hour

// RoleOwner is the role whose holders may write to an agent.
const RoleOwner = "owner"

// Config is the chat section of roomcity.toml, the table [chat]. What is
// particular to a chat system is in the table of its driver.
type Config struct {
	// Name is the city's name, which the space is called.
	Name string `toml:"name"`
	// Transcripts is the address that a session's name is added to for the
	// link to its transcript, such as "https://sandbox.gs.sarak.sh/-/". With
	// none, the journal links no transcript.
	Transcripts string `toml:"transcripts"`
	// Holders maps chat ids onto roles. An id is opaque here: the driver says
	// whether it is one of its own.
	Holders []Holder `toml:"holders"`
	// CatchUp is how old an event may be and still be shown when the room is
	// brought up to date after the server or the chat system was down, such
	// as "1h"; what is older is left out, and the room says so. 0 is
	// DefaultCatchUp.
	CatchUp Duration `toml:"catch_up"`
}

// Holder is a chat id and the role it holds in the city.
type Holder struct {
	ID   string `toml:"id"`
	Role string `toml:"role"`
}

// Validate reports the first thing wrong with the configuration.
func (c Config) Validate() error {
	if c.CatchUp < 0 {
		return fmt.Errorf("chat: catch_up is negative")
	}
	for _, h := range c.Holders {
		if strings.TrimSpace(h.ID) == "" {
			return fmt.Errorf("chat: a holder has no id")
		}
		if h.Role != RoleOwner {
			return fmt.Errorf("chat: holder %q has the role %q, and only %q exists", h.ID, h.Role, RoleOwner)
		}
	}
	return nil
}

// CatchUpWindow returns CatchUp, or DefaultCatchUp where it is 0.
func (c Config) CatchUpWindow() time.Duration {
	if c.CatchUp == 0 {
		return DefaultCatchUp
	}
	return c.CatchUp.Duration()
}

// Owners returns the ids that hold the owner's role.
func (c Config) Owners() []UserID {
	var ids []UserID
	for _, h := range c.Holders {
		if h.Role == RoleOwner {
			ids = append(ids, UserID(h.ID))
		}
	}
	return ids
}

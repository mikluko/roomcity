package chat

import (
	"fmt"
	"strings"
)

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
}

// Holder is a chat id and the role it holds in the city.
type Holder struct {
	ID   string `toml:"id"`
	Role string `toml:"role"`
}

// Validate reports the first thing wrong with the configuration.
func (c Config) Validate() error {
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

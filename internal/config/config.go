// Package config loads roomcity.toml, the configuration of a city's runtime,
// from the city's repository.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/pelletier/go-toml/v2"

	"sarak.sh/mikluko/roomcity/internal/chat"
	"sarak.sh/mikluko/roomcity/internal/chat/matrix"
	"sarak.sh/mikluko/roomcity/internal/kube"
)

// Config is the whole of roomcity.toml. A key the file has and a struct here
// does not is an error, so that a misspelt key is not read as a default.
type Config struct {
	Server Server `toml:"server"`
	// Agents names the agents whose sessions are always-on; the sessions of
	// any other agent are started for a task.
	Agents map[string]Agent `toml:"agents"`
	// Kube is how a session's Job is made.
	Kube kube.Config `toml:"kube"`
	// Chat is the chat side's section, Matrix the table of the driver it
	// works through; Matrix is nil when the file has none.
	Chat   chat.Config    `toml:"chat"`
	Matrix *matrix.Config `toml:"matrix"`
}

// Server is the table [server].
type Server struct {
	// Data is the directory the server keeps its database in.
	Data string `toml:"data"`
	// CityDir is the city's directory, where the real gc runs.
	CityDir string `toml:"city_dir"`
	// Gc is the program run for GcService; "gc" is on the PATH.
	Gc string `toml:"gc"`
	// URL is the address a session's pod reaches the server at.
	URL string `toml:"url"`
	// Listen is the address of the listener the session pods and the
	// homeserver reach. RuntimeListen and PageListen are loopback addresses:
	// the first is for gc's supervisor alone, the second is the upstream of
	// the pod's authenticating proxy.
	Listen        string `toml:"listen"`
	RuntimeListen string `toml:"runtime_listen"`
	PageListen    string `toml:"page_listen"`
	// StartTimeout is how long a start waits for the agent to answer, and
	// StopGrace how long a stop waits for the host to take its instruction,
	// both as a duration such as "100s".
	StartTimeout Duration `toml:"start_timeout"`
	StopGrace    Duration `toml:"stop_grace"`
}

// Agent is the table [agents.<name>].
type Agent struct {
	AlwaysOn bool `toml:"always_on"`
	// Secrets are mounted in the pods of this agent's sessions and of no
	// other's, on top of those of [kube].
	Secrets []kube.Secret `toml:"secrets"`
}

// AgentSecrets returns the Secrets each agent's sessions get of their own.
func (c Config) AgentSecrets() map[string][]kube.Secret {
	out := map[string][]kube.Secret{}
	for name, a := range c.Agents {
		if len(a.Secrets) > 0 {
			out[name] = a.Secrets
		}
	}
	return out
}

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

// Defaults of the keys the file may leave out.
const (
	DefaultListen        = ":8080"
	DefaultRuntimeListen = "127.0.0.1:8082"
	DefaultPageListen    = "127.0.0.1:8081"
)

// Load reads and validates the file at path.
func Load(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	return Parse(b)
}

// Parse reads and validates the configuration in b.
func Parse(b []byte) (Config, error) {
	var c Config
	dec := toml.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		var strict *toml.StrictMissingError
		if errors.As(err, &strict) {
			return Config{}, fmt.Errorf("roomcity.toml: %s", strict.String())
		}
		return Config{}, fmt.Errorf("roomcity.toml: %w", err)
	}
	c.defaults()
	if err := c.validate(); err != nil {
		return Config{}, fmt.Errorf("roomcity.toml: %w", err)
	}
	return c, nil
}

func (c *Config) defaults() {
	s := &c.Server
	if s.Gc == "" {
		s.Gc = "gc"
	}
	if s.Listen == "" {
		s.Listen = DefaultListen
	}
	if s.RuntimeListen == "" {
		s.RuntimeListen = DefaultRuntimeListen
	}
	if s.PageListen == "" {
		s.PageListen = DefaultPageListen
	}
}

func (c *Config) validate() error {
	s := c.Server
	switch {
	case s.Data == "":
		return errors.New("server.data is required")
	case !filepath.IsAbs(s.Data):
		return errors.New("server.data must be an absolute path")
	case s.CityDir == "":
		return errors.New("server.city_dir is required")
	case s.URL == "":
		return errors.New("server.url is required")
	case c.Kube.Namespace == "" || c.Kube.Image == "":
		return errors.New("kube.namespace and kube.image are required")
	case s.StartTimeout < 0 || s.StopGrace < 0:
		return errors.New("a timeout is negative")
	}
	for key, addr := range map[string]string{"server.runtime_listen": s.RuntimeListen, "server.page_listen": s.PageListen} {
		if err := loopback(addr); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
	}
	if err := c.Kube.Validate(); err != nil {
		return err
	}
	for i, sec := range c.Kube.Secrets {
		if err := sec.Validate(); err != nil {
			return fmt.Errorf("kube.secrets[%d]: %w", i, err)
		}
	}
	for agent, a := range c.Agents {
		for i, sec := range a.Secrets {
			if err := sec.Validate(); err != nil {
				return fmt.Errorf("agents.%s.secrets[%d]: %w", agent, i, err)
			}
		}
	}
	if err := c.Chat.Validate(); err != nil {
		return err
	}
	if c.Matrix != nil {
		return c.Matrix.Validate(c.Chat.Owners())
	}
	return nil
}

// loopback returns an error unless addr names a loopback address.
func loopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("%q is not a loopback address", addr)
	}
	return nil
}

// AlwaysOn says whether the agent's sessions are always-on.
func (c Config) AlwaysOn(agent string) bool { return c.Agents[agent].AlwaysOn }

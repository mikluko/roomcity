// Package config loads roomcity.toml, the configuration of a city's runtime,
// from the city's repository.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
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
	// TranscriptTerm is how long a session's transcript is kept after the
	// session ends, as a duration such as "2160h"; 0 means 90 days.
	TranscriptTerm Duration `toml:"transcript_term"`
	// PurgeEvery is how often the transcripts past their term are removed,
	// as a duration such as "1h"; 0 means an hour.
	PurgeEvery Duration `toml:"purge_every"`
}

// Agent is the table [agents.<name>].
type Agent struct {
	AlwaysOn bool `toml:"always_on"`
	// Process says that what gc starts for this agent is no agent session
	// but a service, which the server runs as a process of its own, with no
	// room. The table's name is a pattern (path.Match) of the agent's name
	// as gc gives it in GC_AGENT, so "core.*" names every service of the
	// core pack.
	Process bool `toml:"process"`
	// Secrets are mounted in the pods of this agent's sessions and of no
	// other's, on top of those of [kube].
	Secrets []kube.Secret `toml:"secrets"`
	// Tools are the tools of the room's MCP server, besides attach, that this
	// agent's sessions have, of RoomTools: the foreman's dispatch, wait and
	// result, a worker's claim and done.
	Tools []string `toml:"tools"`
}

// RoomTools are the tools of the room that an agent has only when the
// configuration gives them: they hand work between sessions through gc.
var RoomTools = []string{"dispatch", "wait", "result", "claim", "done"}

// AgentTools returns the tools of the room that the agent's sessions have.
func (c Config) AgentTools(agent string) []string { return c.Agents[agent].Tools }

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
type Duration = chat.Duration

// Defaults of the keys the file may leave out.
const (
	DefaultListen         = ":8080"
	DefaultRuntimeListen  = "127.0.0.1:8082"
	DefaultPageListen     = "127.0.0.1:8081"
	DefaultTranscriptTerm = 90 * 24 * time.Hour
	DefaultPurgeEvery     = time.Hour
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
	if s.TranscriptTerm == 0 {
		s.TranscriptTerm = Duration(DefaultTranscriptTerm)
	}
	if s.PurgeEvery == 0 {
		s.PurgeEvery = Duration(DefaultPurgeEvery)
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
	case s.StartTimeout < 0 || s.StopGrace < 0 || s.TranscriptTerm < 0 || s.PurgeEvery < 0:
		return errors.New("a timeout or a term is negative")
	}
	for name, a := range c.Agents {
		for _, tool := range a.Tools {
			if !slices.Contains(RoomTools, tool) {
				return fmt.Errorf("agents.%s.tools: %q is not a tool of the room; the tools are %s", name, tool, strings.Join(RoomTools, ", "))
			}
		}
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
		if err := c.Kube.CheckMount(sec); err != nil {
			return fmt.Errorf("kube.secrets[%d]: %w", i, err)
		}
	}
	for agent, a := range c.Agents {
		if _, err := path.Match(agent, ""); err != nil {
			return fmt.Errorf("agents.%q is not a pattern: %w", agent, err)
		}
		if a.Process && (a.AlwaysOn || len(a.Secrets) > 0) {
			return fmt.Errorf("agents.%q is a process, which has no room and takes no Secret of its own", agent)
		}
		for i, sec := range a.Secrets {
			if err := sec.Validate(); err != nil {
				return fmt.Errorf("agents.%s.secrets[%d]: %w", agent, i, err)
			}
			if err := c.Kube.CheckMount(sec); err != nil {
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

// IsProcess says whether what gc starts for the agent is run as a process of
// the server.
func (c Config) IsProcess(agent string) bool {
	for pattern, a := range c.Agents {
		if ok, _ := path.Match(pattern, agent); ok && a.Process {
			return true
		}
	}
	return false
}

// AlwaysOn says whether the agent's sessions are always-on.
func (c Config) AlwaysOn(agent string) bool { return c.Agents[agent].AlwaysOn }

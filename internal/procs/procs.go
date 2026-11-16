// Package procs runs what gc starts through the runtime that is not an
// agent, the service agents of the packs, as child processes of the server.
// They have no room and no ACP: a process lives until it ends or is stopped.
package procs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Spec is what to run: gc's start config for a session that is no agent.
type Spec struct {
	// Command is gc's launch command line, run by sh -c as gc's own launch
	// runs it.
	Command string
	// WorkDir is the directory it runs in; Start makes it when it does not
	// exist, as gc stages none for a runtime that is not on its filesystem.
	WorkDir string
	// Env is applied over the server's environment; an empty value unsets
	// the variable, as gc asks.
	Env map[string]string
}

// Manager runs the processes. A process that ends by itself is forgotten, so
// the name is free to start again. The processes live in the server's
// container and end with it.
type Manager struct {
	// LogDir holds each process's output, in LogDir/<name>.log. The log is
	// cut at MaxLog bytes (DefaultMaxLog when zero): the file is kept as
	// <name>.log.1 and a new one is begun, so a process's log is held to
	// twice that and nobody trims it.
	LogDir string
	MaxLog int64
	// Grace is how long Stop waits after SIGTERM before SIGKILL; zero is
	// five seconds.
	Grace time.Duration

	mu    sync.Mutex
	procs map[string]*proc
}

type proc struct {
	cmd     *exec.Cmd
	done    chan struct{}
	started time.Time
}

// ErrExists is returned for a name that has a process running.
var ErrExists = errors.New("procs: process already exists")

// Start runs spec under name. It returns once the process has been spawned.
func (m *Manager) Start(name string, spec Spec) error {
	if name == "" || strings.ContainsAny(name, "/\\") || name == "." || name == ".." {
		return fmt.Errorf("procs: %q is not a name", name)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.procs[name]; ok {
		select {
		case <-p.done:
		default:
			return ErrExists
		}
	}
	if err := os.MkdirAll(m.LogDir, 0o750); err != nil {
		return err
	}
	if err := os.MkdirAll(spec.WorkDir, 0o755); err != nil {
		return err
	}
	max := m.MaxLog
	if max == 0 {
		max = DefaultMaxLog
	}
	log, err := openCapped(filepath.Join(m.LogDir, name+".log"), max)
	if err != nil {
		return err
	}
	cmd := exec.Command("sh", "-c", spec.Command)
	cmd.Dir = spec.WorkDir
	cmd.Env = environ(os.Environ(), spec.Env)
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		_ = log.Close()
		return err
	}
	p := &proc{cmd: cmd, done: make(chan struct{}), started: time.Now()}
	if m.procs == nil {
		m.procs = map[string]*proc{}
	}
	m.procs[name] = p
	go func() {
		_ = cmd.Wait()
		_ = log.Close()
		close(p.done)
	}()
	return nil
}

// Info describes the process of the name, which was started and has not been
// stopped: the one that ended on its own is held until it is stopped or
// started again.
type Info struct {
	Held    bool
	Alive   bool
	PID     int
	Started time.Time
}

// Info returns what the Manager holds under name.
func (m *Manager) Info(name string) Info {
	m.mu.Lock()
	p, ok := m.procs[name]
	m.mu.Unlock()
	if !ok {
		return Info{}
	}
	in := Info{Held: true, PID: p.cmd.Process.Pid, Started: p.started}
	select {
	case <-p.done:
	default:
		in.Alive = true
	}
	return in
}

// Held lists the names that hold a process, live or dead, sorted.
func (m *Manager) Held() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	names := make([]string, 0, len(m.procs))
	for n := range m.procs {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// Alive reports whether a process runs under name.
func (m *Manager) Alive(name string) bool {
	m.mu.Lock()
	p, ok := m.procs[name]
	m.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// Running lists the names that have a process running, sorted.
func (m *Manager) Running() []string {
	m.mu.Lock()
	names := make([]string, 0, len(m.procs))
	for n := range m.procs {
		names = append(names, n)
	}
	m.mu.Unlock()
	var out []string
	for _, n := range names {
		if m.Alive(n) {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

// Stop ends the process and everything it started, with SIGTERM and, after
// the grace, SIGKILL, and returns once it is gone. A name with no process is
// stopped all the same.
func (m *Manager) Stop(ctx context.Context, name string) error {
	m.mu.Lock()
	p, ok := m.procs[name]
	m.mu.Unlock()
	if !ok {
		return nil
	}
	grace := m.Grace
	if grace == 0 {
		grace = 5 * time.Second
	}
	pgid := p.cmd.Process.Pid
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(grace):
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		select {
		case <-p.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	case <-ctx.Done():
		return ctx.Err()
	}
	// What the process left in its group dies with it.
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	m.mu.Lock()
	if m.procs[name] == p {
		delete(m.procs, name)
	}
	m.mu.Unlock()
	return nil
}

// Tail returns the last lines of the output of the process, all of them for
// lines <= 0; it is nothing for a name that never had one. It reads the
// current file of the log, not the one before the cut.
func (m *Manager) Tail(name string, lines int) []string {
	b, err := os.ReadFile(filepath.Join(m.LogDir, name+".log"))
	if err != nil {
		return nil
	}
	out := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if lines > 0 && len(out) > lines {
		out = out[len(out)-lines:]
	}
	return out
}

// environ is base with the variables of set applied over it, an empty value
// removing the variable.
func environ(base []string, set map[string]string) []string {
	out := make([]string, 0, len(base)+len(set))
	for _, kv := range base {
		name, _, _ := strings.Cut(kv, "=")
		if _, over := set[name]; !over {
			out = append(out, kv)
		}
	}
	for k, v := range set {
		if v != "" {
			out = append(out, k+"="+v)
		}
	}
	return out
}

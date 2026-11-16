package host

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"sarak.sh/mikluko/claudeacp/claudeacp"
)

// agent is a running ACP agent: what the host reads of it, what it writes,
// and the end of it, which carries what ended it.
type agent struct {
	r io.Reader
	w io.WriteCloser
	// ended is closed when the agent has ended, and err is then how.
	ended chan struct{}
	err   error
	// stderr holds the last of what a process agent wrote on its standard
	// error; nil for the agent in this process.
	stderr *tail
	// stderrRead is closed when stderr has been read to its end, which is
	// after the process has gone: the agent's last words can be behind its end.
	stderrRead chan struct{}
}

func newAgent(r io.Reader, w io.WriteCloser) *agent {
	return &agent{r: r, w: w, ended: make(chan struct{})}
}

func (a *agent) end(err error) {
	a.err = err
	close(a.ended)
}

// wait returns how the agent ended, and false when it has not within d.
func (a *agent) wait(d time.Duration) (error, bool) {
	select {
	case <-a.ended:
		return a.err, true
	case <-time.After(d):
		return nil, false
	}
}

// tail keeps the last size bytes written to it.
type tail struct {
	mu   sync.Mutex
	size int
	b    []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > t.size {
		t.b = t.b[len(t.b)-t.size:]
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.b))
}

// whyEnded says what the agent had to tell of its end: how it ended, and the
// last of its standard error. An agent that has not ended within a moment is
// not blamed. The text goes into the host's error and its log, so that a host
// whose agent died at once does not say only that the connection closed.
func (a *agent) whyEnded() string {
	err, ended := a.wait(agentWordGrace)
	var parts []string
	switch {
	case !ended:
		return ""
	case err != nil:
		parts = append(parts, "the agent ended: "+err.Error())
	default:
		parts = append(parts, "the agent ended without an error")
	}
	if a.stderr != nil {
		select {
		case <-a.stderrRead:
		case <-time.After(stderrGrace):
		}
		if s := a.stderr.String(); s != "" {
			parts = append(parts, "its standard error: "+s)
		}
	}
	return strings.Join(parts, "; ")
}

// agentWordGrace is how long the host waits for an agent that closed its
// connection to be seen to end.
const agentWordGrace = 5 * time.Second

// stderrGrace is how long the host waits, after the agent has ended, for the
// last of its standard error: a process the agent left running can hold the
// pipe open.
const stderrGrace = time.Second

// stderrTail is how much of an agent's standard error an error keeps.
const stderrTail = 4 << 10

// SecretPrefix starts the name of each variable of the host's environment that
// names a file holding a secret of the agent: ROOMCITY_SECRET_<NAME>=<path>
// gives the agent the variable NAME, with the file's content. The server writes
// them, one for each secret its configuration gives a variable name.
const SecretPrefix = "ROOMCITY_SECRET_"

// agentEnv is the environment of an agent: env, with each variable that a
// SecretPrefix entry names set to the content of its file. A file that cannot
// be read, or is empty, is an error that names the variable and the path; the
// content is never part of it.
func agentEnv(env []string) ([]string, error) {
	out := append([]string(nil), env...)
	for _, kv := range env {
		name, path, _ := strings.Cut(kv, "=")
		variable, ok := strings.CutPrefix(name, SecretPrefix)
		if !ok || variable == "" {
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("host: the file of %s, %s: %w", variable, path, err)
		}
		value := strings.TrimSpace(string(b))
		if value == "" {
			return nil, fmt.Errorf("host: the file of %s, %s, is empty", variable, path)
		}
		out = slices.DeleteFunc(out, func(kv string) bool { return strings.HasPrefix(kv, variable+"=") })
		out = append(out, variable+"="+value)
	}
	return out, nil
}

// runsClaude says whether command is Claude Code's, which the host drives
// through claudeacp in its own process. Any other command is an ACP agent of
// its own, a process that speaks ACP on its standard streams.
func runsClaude(command []string) bool {
	return len(command) == 0 || filepath.Base(command[0]) == "claude"
}

// startAgent starts the agent command names. The program of Claude Code's
// command is not run, only the words after it are claude's arguments.
func (h *host) startAgent(ctx context.Context, command []string) (*agent, error) {
	if runsClaude(command) {
		cfg := h.cfg.Claude
		// The tools whose calls claudeacp sends as plans: Claude Code has none
		// without a terminal, and the room shows a plan.
		cfg.TaskTools = true
		if cfg.Log == nil {
			cfg.Log = h.log
		}
		if len(command) > 1 {
			cfg.ClaudeArgs = append(append([]string(nil), cfg.ClaudeArgs...), withoutSessionFlags(command[1:])...)
		}
		// claudeacp starts claude with this process's environment.
		env, err := agentEnv(os.Environ())
		if err != nil {
			return nil, err
		}
		for _, kv := range env {
			name, value, _ := strings.Cut(kv, "=")
			if os.Getenv(name) != value {
				_ = os.Setenv(name, value)
			}
		}
		return h.startClaude(ctx, cfg), nil
	}
	return h.startProcess(ctx, command)
}

// withoutSessionFlags drops the flags gc adds to name, resume or fork claude's
// session, with their values where they have one: claudeacp names, resumes and
// forks each session itself and refuses them.
func withoutSessionFlags(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--session-id" || a == "--resume" || a == "-r":
			i++
		case strings.HasPrefix(a, "--session-id=") || strings.HasPrefix(a, "--resume="):
		case a == "--fork-session":
		default:
			out = append(out, a)
		}
	}
	return out
}

// startClaude serves claudeacp in this process, over two pipes.
func (h *host) startClaude(ctx context.Context, cfg claudeacp.Config) *agent {
	toAgentR, toAgentW := io.Pipe()
	fromAgentR, fromAgentW := io.Pipe()
	a := newAgent(fromAgentR, toAgentW)
	go func() {
		err := claudeacp.Serve(ctx, toAgentR, fromAgentW, cfg)
		_ = fromAgentW.Close()
		a.end(err)
	}()
	return a
}

// processGrace is how long an agent process has to end after its input is
// closed, and after it is asked to end, before it is killed.
const processGrace = 5 * time.Second

// startProcess runs command as the ACP agent, in the working directory and
// the host's environment. Its stderr is the host's log, a line at a time.
func (h *host) startProcess(ctx context.Context, command []string) (*agent, error) {
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Dir = h.cfg.WorkDir
	env, err := agentEnv(os.Environ())
	if err != nil {
		return nil, err
	}
	cmd.Env = env
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = processGrace
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	// The output pipes are ours, not the command's: os/exec closes the ones it
	// makes when the process exits, and the host may not yet have read what
	// the agent wrote last. Ours end when the process has gone and its output
	// has been read.
	stdout, outW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	stderr, errW, err := os.Pipe()
	if err != nil {
		_ = stdout.Close()
		_ = outW.Close()
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = outW, errW
	err = cmd.Start()
	_ = outW.Close() // the process holds the write ends now
	_ = errW.Close()
	if err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("host: starting %s: %w", command[0], err)
	}
	a := newAgent(stdout, stdin)
	a.stderr = &tail{size: stderrTail}
	a.stderrRead = make(chan struct{})
	go func() {
		defer close(a.stderrRead)
		defer func() { _ = stderr.Close() }()
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			_, _ = a.stderr.Write(append(sc.Bytes(), '\n'))
			h.log.Info("agent", "stderr", sc.Text())
		}
	}()
	go func() { a.end(cmd.Wait()) }()
	return a, nil
}

package host

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"sarak.sh/mikluko/claudeacp/claudeacp"
)

// agent is a running ACP agent: what the host reads of it, what it writes,
// and the end of it, which carries what ended it.
type agent struct {
	r    io.Reader
	w    io.WriteCloser
	done chan error
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
		if len(command) > 1 {
			cfg.ClaudeArgs = append(append([]string(nil), cfg.ClaudeArgs...), command[1:]...)
		}
		return h.startClaude(ctx, cfg), nil
	}
	return h.startProcess(ctx, command)
}

// startClaude serves claudeacp in this process, over two pipes.
func (h *host) startClaude(ctx context.Context, cfg claudeacp.Config) *agent {
	toAgentR, toAgentW := io.Pipe()
	fromAgentR, fromAgentW := io.Pipe()
	a := &agent{r: fromAgentR, w: toAgentW, done: make(chan error, 1)}
	go func() {
		err := claudeacp.Serve(ctx, toAgentR, fromAgentW, cfg)
		_ = fromAgentW.Close()
		a.done <- err
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
	cmd.Env = os.Environ()
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
	go func() {
		defer func() { _ = stderr.Close() }()
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			h.log.Info("agent", "stderr", sc.Text())
		}
	}()
	a := &agent{r: stdout, w: stdin, done: make(chan error, 1)}
	go func() { a.done <- cmd.Wait() }()
	return a, nil
}

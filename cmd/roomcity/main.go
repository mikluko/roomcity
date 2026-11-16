// Command roomcity is the session runtime room of Gas City in one binary:
// "roomcity runtime" is the exec session runtime gc calls, "roomcity server"
// the server beside gc's supervisor, "roomcity host" the host of one session
// in its pod, and "roomcity gc" the stand-in for gc in that pod.
//
// Run as gc-runtime-room it is "roomcity runtime", and run as gc it is
// "roomcity gc": gc takes an executable for its runtime, not a command line,
// and a session's hooks call gc by that name.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"sarak.sh/mikluko/roomcity/internal/standin"
)

const (
	exitOK = 0
	// exitFailure is the code of a failed operation. It is never 2, which gc
	// reads from its runtime as an operation the runtime does not know and
	// treats as success.
	exitFailure = 1
	// exitUsage is EX_USAGE of sysexits.h.
	exitUsage = 64
)

const usage = `usage: roomcity <command> [<args>...]

commands:
  runtime   Gas City's exec session runtime: roomcity runtime <operation> [<args>...]
  server    the runtime's server, beside gc's supervisor
  host      the host of one session, in the session's pod
  gc        the stand-in for gc in a session's pod: roomcity gc [<args>...]
`

// aliases maps a program name the binary is installed under to the command
// it runs as.
var aliases = map[string]string{
	"gc-runtime-room": "runtime",
	"gc":              "gc",
}

// process is what a command has of the process it runs in.
type process struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	// Env is the environment, as os.Environ gives it.
	Env []string
	// Dir is the working directory.
	Dir string
}

// command runs with ctx, which ends on SIGINT or SIGTERM, and returns the
// exit code.
type command func(ctx context.Context, args []string, p process) int

// commands holds every command of the binary.
var commands = map[string]command{
	"runtime": notImplemented("runtime"),
	"server":  notImplemented("server"),
	"host":    notImplemented("host"),
	"gc":      runGc,
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	dir, _ := os.Getwd()
	os.Exit(run(ctx, os.Args, process{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Env: os.Environ(), Dir: dir}))
}

// run dispatches argv, whose first element is the program name, and returns
// the exit code.
func run(ctx context.Context, argv []string, p process) int {
	name, args := resolve(argv)
	switch name {
	case "":
		_, _ = io.WriteString(p.Stderr, usage)
		return exitUsage
	case "-h", "-help", "--help", "help":
		_, _ = io.WriteString(p.Stdout, usage)
		return exitOK
	}
	run, ok := commands[name]
	if !ok {
		_, _ = fmt.Fprintf(p.Stderr, "roomcity: unknown command %q\n%s", name, usage)
		return exitUsage
	}
	return run(ctx, args, p)
}

// resolve returns the command argv names and the arguments after it. The
// command is the alias of the program name where it has one, else the first
// argument; it is "" when argv names none.
func resolve(argv []string) (string, []string) {
	if len(argv) == 0 {
		return "", nil
	}
	if name, ok := aliases[filepath.Base(argv[0])]; ok {
		return name, argv[1:]
	}
	if len(argv) == 1 {
		return "", nil
	}
	return argv[1], argv[2:]
}

// notImplemented returns a command that fails, saying that name does nothing.
func notImplemented(name string) command {
	return func(_ context.Context, _ []string, p process) int {
		_, _ = fmt.Fprintf(p.Stderr, "roomcity %s: not implemented\n", name)
		return exitFailure
	}
}

// runGc is the stand-in for gc in a session's pod.
func runGc(ctx context.Context, args []string, p process) int {
	cfg, err := standin.ConfigFromEnv(p.Env)
	if err != nil {
		_, _ = fmt.Fprintf(p.Stderr, "gc: %v\n", err)
		return exitFailure
	}
	return standin.Run(ctx, cfg, args, standin.Streams{Stdin: p.Stdin, Stdout: p.Stdout, Stderr: p.Stderr, Env: p.Env, Dir: p.Dir})
}

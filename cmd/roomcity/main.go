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
	"fmt"
	"io"
	"os"
	"path/filepath"
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

// commands holds every command of the binary.
var commands = map[string]func(args []string, stderr io.Writer) int{
	"runtime": notImplemented("runtime"),
	"server":  notImplemented("server"),
	"host":    notImplemented("host"),
	"gc":      notImplemented("gc"),
}

func main() {
	os.Exit(run(os.Args, os.Stdout, os.Stderr))
}

// run dispatches argv, whose first element is the program name, and returns
// the exit code.
func run(argv []string, stdout, stderr io.Writer) int {
	name, args := resolve(argv)
	switch name {
	case "":
		_, _ = io.WriteString(stderr, usage)
		return exitUsage
	case "-h", "-help", "--help", "help":
		_, _ = io.WriteString(stdout, usage)
		return exitOK
	}
	command, ok := commands[name]
	if !ok {
		_, _ = fmt.Fprintf(stderr, "roomcity: unknown command %q\n%s", name, usage)
		return exitUsage
	}
	return command(args, stderr)
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
func notImplemented(name string) func([]string, io.Writer) int {
	return func(_ []string, stderr io.Writer) int {
		_, _ = fmt.Fprintf(stderr, "roomcity %s: not implemented\n", name)
		return exitFailure
	}
}

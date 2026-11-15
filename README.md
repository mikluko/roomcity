# roomcity

roomcity is a [Gas City](https://github.com/gastownhall/gascity) pack that adds the session runtime `room`.
The runtime is to drive each agent session over the [Agent Client Protocol](https://github.com/agentclientprotocol), with no terminal, and to give each session a chat room as its interface.

Nothing of that works yet.
This version holds the parts and none of their behaviour: every command of the binary fails with "not implemented".

## The parts

One binary, `roomcity`, is every program of the runtime:

| Command            | Also runs as      | Is                                                        |
| ------------------ | ----------------- | --------------------------------------------------------- |
| `roomcity runtime` | `gc-runtime-room` | the exec session runtime gc calls, once for each operation |
| `roomcity server`  |                   | the server, which runs beside gc's supervisor             |
| `roomcity host`    |                   | the host of one session, in the session's pod             |
| `roomcity gc`      | `gc`              | the stand-in for gc in a session's pod                    |

gc takes an executable for its runtime, not a command line, so `gc-runtime-room` is a link to `roomcity`.
`pack.toml` declares the runtime under the name `room`.

The three programs talk to the server over [ConnectRPC](https://connectrpc.com).
The protocol is in `proto/roomcity/v1`, one file for each caller:

| File            | Service          | Called by         |
| --------------- | ---------------- | ----------------- |
| `runtime.proto` | `RuntimeService` | `gc-runtime-room` |
| `host.proto`    | `HostService`    | the session host  |
| `gc.proto`      | `GcService`      | the stand-in gc   |

## The image

`sarak.sh/mikluko/roomcity` is the one image gc's supervisor, the server and the sessions' pods run from.
It carries `roomcity`, `gc`, `bd`, `dolt`, `git`, Claude Code and goose, and runs as the user `city`, uid 1000.

| Path                             | Is                                                |
| -------------------------------- | ------------------------------------------------- |
| `/usr/local/bin/gc`              | gc, built from a pinned commit of its `main`      |
| `/usr/local/bin/gc-runtime-room` | a link to `roomcity`                              |
| `/usr/local/lib/roomcity/bin/gc` | the stand-in gc; a session's pod puts this directory first on `PATH` |

The `Dockerfile` pins every version.

## Develop

Go 1.27.4 or later and [buf](https://buf.build/docs/cli/installation/) are required.

| Command         | Does                                                           |
| --------------- | -------------------------------------------------------------- |
| `make generate` | regenerates `internal/gen` from `proto/`                       |
| `make lint`     | runs `buf lint`, `buf format` and `go vet`                     |
| `make test`     | runs the tests                                                 |
| `make image`    | builds the image with Apple's `container` and runs its tools inside it |

## Release

A push to `main` releases when the newest entry of `CHANGELOG.md` names a version that has no tag.
The workflow `release` builds the image for amd64 and arm64, pushes it as `X.Y.Z`, `X.Y` and `latest`, and then creates the tag `vX.Y.Z`.

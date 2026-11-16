# roomcity

roomcity is a [Gas City](https://github.com/gastownhall/gascity) pack that adds the session runtime `room`.
The runtime is to drive each agent session over the [Agent Client Protocol](https://github.com/agentclientprotocol), with no terminal, and to give each session a chat room as its interface.

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

## An example

`examples/minimal` holds the configuration of a city as a minimal example of a deployment: the city's repository, the manifests of its place in a Kubernetes cluster and the registration for the homeserver. Its README says what must be there before it and what to replace.

## What a session's pod is given

A session is a Kubernetes Job of one pod in the server's namespace.
Its environment is what gc set for the agent (the supervisor's variables and those of `city.toml`'s workspace, provider and agent), less what a pod has of its own (`PATH`, `HOME`, `USER`, `LOGNAME`, `SHELL`, `PWD`, `OLDPWD`, `TMPDIR`, `HOSTNAME`, `TERM`, the locale (`LANG`, `LANGUAGE`, `LC_*`), `XDG_*`, `KUBERNETES_*`) and the two names of the Dolt password gc sets (`GC_DOLT_PASSWORD`, `BEADS_DOLT_PASSWORD`; bd reads its credentials from the file `BEADS_CREDENTIALS_FILE` names), plus the server's own `ROOMCITY_*`, among them the session's token.

None of that is written into the Job's spec, which whoever may read Jobs can read.
The server makes a Secret for each session, named like its Job, which holds every one of those variables; the pod reads it with `envFrom`, the Job owns it, and deleting the Job, as stopping the session does, removes it.
The Secrets of the namespace that `roomcity.toml` lists (`[kube] secrets`, `[agents.<name>] secrets`) are mounted as files, and a file's path may be handed to the pod as a variable (`path_env`, `env`); a credential that is not in gc's environment reaches a pod that way.

Who can read a session's Secret: whoever may read Secrets in the namespace, which the server's Role grants it to make (create, update and delete on `secrets`: it cannot read or list them).
A server that lacks the right fails the start and says so; it does not fall back to a plain environment.
So an API key put in `city.toml`'s `env` reaches the pod and is readable by the namespace's readers of Secrets, and not by its readers of Jobs.

Through gc a session can do everything gc's command line does in the city, stopping and starting the other sessions included: the server runs gc for it, in the city, with the server's own environment, and sets the variables by which gc knows who is calling (the eleven of `session.IdentityEnv`: `GC_AGENT`, `GC_ALIAS`, `GC_SESSION_ID`, `GC_SESSION_NAME`, `GC_TEMPLATE`, `GC_SESSION_ORIGIN`, `GC_INSTANCE_TOKEN`, `GC_CONTINUATION_EPOCH`, `GC_RUNTIME_EPOCH`, `GC_PROVIDER`, `GC_STARTUP_PROMPT_DELIVERED`) from what the server kept of the session's start, never from what the session sends. A session that was started before the server kept it is the exception, until it is started again: its own word is taken for those variables.

Two limits, from the server's not being able to read or list Secrets:

- A server that dies between making a session's Secret and making its Job leaves a Secret that nothing owns. The server cannot list Secrets to find it; the next start of the same session overwrites it and a stop of that name deletes it, and otherwise it stays until someone deletes it.
- Starts of one name are serialized by the server and only the server makes a session's Job, so a Job that appears between the start's check for one and its own creation, made by hand, would find its Secret overwritten.

## The database and its upgrades

The server keeps the sessions, their events, their queues and the transcripts in one SQLite database, `roomcity.db` in `[server] data`, and its layout has a version (`PRAGMA user_version`).
A server that opens a database of an earlier version migrates it, in one transaction, and a server that opens one of a later version refuses it and does not start.

Before it migrates, the server writes SQLite's own consistent copy of the database as it was, beside it, under a name that carries the old version and the time: `roomcity.v1.20261116T120000Z.db` for a migration from version 1 at that time.
It logs the path; the copy is whole when it is there, and the migration goes on only after it.
A copy is never in the server's way: the name is of the second, takes a number when it is taken, and no copy that exists is replaced or makes the server refuse to start.
A migration that fails removes the copy of its own try.
The server keeps the copies until an operator removes them.

A rollback to a server of the earlier version needs the copy: the older server refuses the migrated database.
Stop the server, put the copy in place of `roomcity.db` (remove `roomcity.db-wal` and `roomcity.db-shm` with it), and run the older server.
What the migrated database recorded after the copy was made is lost with it.
Lowering `user_version` by hand is not a way back.

## Develop

Go 1.27.4 or later and [buf](https://buf.build/docs/cli/installation/) are required.

| Command         | Does                                                           |
| --------------- | -------------------------------------------------------------- |
| `make generate` | regenerates `internal/gen` from `proto/`                       |
| `make lint`     | runs `buf lint`, `buf format` and `go vet`                     |
| `make test`     | runs the tests                                                 |
| `make image`    | builds the image with Apple's `container` and runs its tools inside it |

The tests of the Matrix driver need a Tuwunel and are skipped without one; CI runs them, with Tuwunel's own binary fetched by `scripts/fetch-tuwunel.sh` (checked against a pinned checksum) and started as a process by `scripts/tuwunel-test.sh run`, and fails if there is none.
`scripts/tuwunel-test.sh up` starts Tuwunel 2.0.0 with Apple's `container` and prints the lines to export: `ROOMCITY_TEST_HOMESERVER`, `ROOMCITY_TEST_LISTEN` and `ROOMCITY_TEST_SERVE_LISTEN` (where the driver tests and the serve test, each a city of its own, take the homeserver's transactions, which the registrations name) and `ROOMCITY_TEST_TUWUNEL`.
`scripts/tuwunel-test.sh down "$ROOMCITY_TEST_TUWUNEL"` removes it.

## Release

A push to `main` releases when the newest entry of `CHANGELOG.md` names a version that has no tag.
The workflow `release` builds the image for amd64 and arm64, pushes it as `X.Y.Z`, `X.Y` and `latest`, and then creates the tag `vX.Y.Z`.

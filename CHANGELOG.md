# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/2.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed

- `inventory` of a city that holds no box answers `"sessions":[]` and not `"sessions":null`, which gc read as an answer without sessions and refused, so that no session of the city could start.

## [0.3.0] - 2026-11-16

### Added

- The runtime's handshake declares `observe.v1` as well, with its operations `observe`, `inventory` and `kill-object`, and `fresh_only` starts (exit 73 for a name any box holds, live or dead). A session's box is its Job, named by the Job's UID and creation time, and `kill-object` deletes by that UID; a service run by the server is a box with its pid. The server answers an agent `dead` only where it can tell: for a service, from the process table; for an agent in a pod, when gc asks about no process the staged command runs. `peek` answers exit 66 for a name no box holds. gc's own `runtime check` passes 21 of 21 and `runtime conformance` 40 of 40 required requirements, and calls the runtime v2-ready.
- A Secret that has a `key` may name `path_env`: a variable the pod's environment holds the path of the mounted file in (bd's `BEADS_CREDENTIALS_FILE`). A Secret mounted inside the home of the image's user (`/home/city`, or `[kube] home`) is refused when `roomcity.toml` is read: the kubelet makes the directories above a mounted file root's, and the agent can then make none of its own beside it (`~/.config`, `~/.claude`).

### Changed

- The records of the owner's prompts that waited are closed with a `Dropped` record also when a session is started afresh or ends, not only at `!stop`.
- A start fails as soon as the session's pod has ended, with the pod's state and the end of its log in the answer gc sees, and not after the 100 seconds it waited. The server's service account needs `get` on `pods/log` for the log.

### Fixed

- `roomcity server` starts the chat side. It had mounted the three services and the transcripts' page and nothing of the rooms, so no room appeared and the homeserver's transactions were answered 404. With `[matrix]` in `roomcity.toml` the server now makes the Matrix driver from it and the tokens in `as_token_file` and `hs_token_file`, mounts the driver's handler at `/_matrix/app/` on the network listener, and runs the chat service beside it, with the transcripts' address from `[chat]`. A `[chat]` without its `[matrix]`, or tokens it cannot read, stops the start with an error.
- A host whose agent ended before or while the session started now says why: how the agent ended, and for a process the last of its standard error, in its error and its log, instead of only that the connection closed.

## [0.2.0] - 2026-11-16

### Changed

- The image's `gc` is built from commit 76a06b2 of gascity's `main`, whose `gc init` checks the dependencies of the runtime the city selects and not tmux's.
- On `Cancel` the host keeps the prompts gc sent, which run in order once the turn has stopped, and drops only the owner's waiting prompts.

### Added

- The Matrix driver's tests run in CI: the job fetches Tuwunel 2.0.0's static binary, checks it against a pinned checksum, and starts it as a process, since the runner has no Docker daemon. `scripts/tuwunel-test.sh run <binary>` starts it, and `scripts/fetch-tuwunel.sh` fetches it. With `ROOMCITY_TEST_REQUIRE_HOMESERVER` set, a test fails instead of skipping when there is no homeserver.

- A session's transcript as a page of the server's page listener: `/-/<session>` shows the session's card and its stream as a room shows it, but whole (a tool call's output and a permission request's detail are not cut, and the owner's own words are in), `/-/<session>/raw` gives the stream as it passed through the server, one JSON message a line, and `/-/` lists the sessions. The page is read-only and sits behind the pod's authenticating proxy; the journal's notice of a session's end links to it. A transcript is removed `transcript_term` after its session ended, 90 days unless the `[server]` table says otherwise; a session still running is never removed.
- At each start the server removes from the city's rooms those who no longer hold a role: anyone in a room the city's own user made, a room that left the space included, who is neither one of the city's own users nor in `[[chat.holders]]`, whether they had joined or only been invited. Nothing they said is redacted. With no holder in the configuration no one is removed.
- The owner's message goes into the running turn at once where the agent takes a message into it, as Claude Code does through steering, and waits for the turn's end where it does not, as goose does: `Prompt.into_turn` in `host.proto` asks it for the owner's prompts, and the new call `HostService.Handled` tells the server whether the host steered a prompt into the turn, began a turn with it, or queued it. The server writes the prompt's Waiting and Delivered records from that report alone, so the room's ⏳ is on exactly the messages that wait.
- The city's space, named for the city, with a room for each always-on agent in it and the journal, which the city's own user writes: a notice with a link to the room when a session for a task starts, one with a link to its transcript when it ends, and one when an always-on agent's session begins anew. A session started for a task has a room of its own, named for the agent and the session and open to the space's members without an invitation; at the session's end the room leaves the space and the agent's user leaves the room. The `name` and `transcripts` keys of `[chat]` set the space's name and the transcripts' address.
- The host gives its agent the secrets the server mounts as files: each variable `ROOMCITY_SECRET_<NAME>=<path>` of its environment sets `NAME` for the agent to the content of that file, and a file that cannot be read ends the host, naming the variable and the path.
- The session's stream in an agent's room as decided: thoughts as one folded notice for each run, a tool call as a notice for its start and one in reply for its result, the plan as one notice edited in place at most every 1.2 s, the typing notice for the whole turn and off while a permission request waits, a permission request as a message with a reaction for each option, answered by the first owner's reaction, and gc's nudge as a notice.
- gc's `interrupt` (and its Escape and Ctrl-C keys) ends the running turn and drops nothing: `Cancel` of `HostService` has `keep_waiting`, and what waits runs after the turn, the owner's first. The owner's `!stop` closes the record of each of the owner's prompts that it drops with a `Dropped` prompt record, so that a room can take the ⏳ off.
- In an always-on agent's room, `!stop`, which cancels the turn, and `!help`; an unknown command is answered with the known ones. A message to a session that is not running is answered with a notice that it was not taken. The reaction ⏳ marks the owner's message while it waits for the turn's end.
- `roomcity host` drives an agent that is an ACP process of its own: when the command the server stages names a program other than `claude`, the host runs it in the working directory and speaks ACP on its standard streams, framing and reporting it as it does Claude Code. Tried on `goose acp` with `kimi-for-coding`.
- `roomcity server`: it opens the registry, reads `roomcity.toml` (`-config`, or `ROOMCITY_CONFIG`) and serves on three listeners: the network (`HostService`, `GcService`), a loopback port for the runtime alone (`server.runtime_listen`, 127.0.0.1:8082) and a loopback port for the transcripts page's proxy (`server.page_listen`, 127.0.0.1:8081). Each session is a Kubernetes Job in the server's namespace.
- The runtime's handshake (`protocol`) declares `carrier.ops`, `idle-boundary`, `report-activity` and `report-attachment`, and the operations they ask for: `peek`, which shows the last lines of what the room shows as text, ending with gc's ready prompt when no turn runs and with a busy line while one does (a turn that waits for a permission answer is not idle); `get-last-activity`, the time of the session's latest frame; `interrupt` and `send-keys` (Escape and Ctrl-C), which end the turn as `!stop` does; `clear-scrollback`; and `is-attached`, always false. gc's `runtime check` passes all 17 checks and `runtime conformance` fails none.
- `roomcity runtime`, also `gc-runtime-room`: gc's exec session runtime. It hands each operation to the server at `ROOMCITY_RUNTIME_URL`. `start`, `stop`, `is-running`, `process-alive`, `list-running`, `nudge` and the meta operations are carried out; the rest answer exit 2, as an operation that is not offered.
- The registry, in SQLite: sessions, one numbering of each session's frames, prompts and lifecycle, the queue of instructions that survives a restart, the marks gc keeps, and the bundle a start stages for the host.
- A Secret of `roomcity.toml` that has a `key` may name `env`, the variable the agent gets the file's content in. The Job then carries `ROOMCITY_SECRET_<VARIABLE>`, the path of the mounted file, and the value is never in the pod's spec.
- What gc starts for a service agent runs as a process of the server, with no Job, no room and no host: `[agents."<pattern>"] process = true` names the agents, by `GC_AGENT` and `path.Match`. The command gc gives is run by `sh -c` in its working directory with its environment, its output goes to `<data>/processes/<name>.log`, and `stop` ends the process and all it started. Such a process is running, listed and stopped as a session is; a nudge to it fails.
- The `[server]`, `[agents.<name>]` and `[kube]` sections of `roomcity.toml`; a key the file has and the program does not know is an error.
- `HostService.Stage`, which the host calls once before it starts the agent: a manifest (the agent's command, the commands to run before and after the start, the setup script) and the files, as a tar stream extracted under the working directory. A server that has not implemented it is taken to stage nothing.
- `roomcity host`, the program of a session's pod: it drives Claude Code through `claudeacp` v0.6.0 in its own process, takes the server's instructions (prompt, cancel, permission answer, stop) and reports every ACP message in both directions as a numbered frame, sending again what the server has not acknowledged. After a lost connection it attaches again, naming the last instruction it took. It reads `ROOMCITY_SESSION`, `ROOMCITY_TOKEN`, `ROOMCITY_SERVER` and `ROOMCITY_WORKDIR`, and `ROOMCITY_SETTINGS`, a settings file for claude, when set.
- The stand-in `gc`, `roomcity gc`: it sends its arguments, standard input, `GC_*` environment and directory to the server through `GcService` and ends with the exit code of the gc the server ran. It takes the session, its token and the server from `ROOMCITY_SESSION`, `ROOMCITY_TOKEN` and `ROOMCITY_SERVER`, and sends the token as a bearer token with the call.
- The server's side of `GcService`, package `internal/gchandler`: it runs the real `gc` in the city's directory with the session's `GC_*` variables, for a call that carries the session's own token.
- The chat side, in `internal/chat`: a driver interface in a chat system's neutral terms, a room for each always-on agent as its session appears, an owner's message in the room as a prompt, and the agent's answer out as one message for each paragraph.
- The Matrix driver, in `internal/chat/matrix`: an application service on mautrix-go whose agents' rooms are found again by alias after a restart, and whose transactions handler is mounted on the server.
- The `[chat]` section of `roomcity.toml`, the holders of the owner's role, and the `[matrix]` section: the homeserver, the server name, the city's name and the token files.
- `scripts/tuwunel-test.sh`, a local Tuwunel 2.0.0 for the driver's tests.

## [0.1.0] - 2026-11-15

### Added

- The binary `roomcity` with the commands `runtime`, `server`, `host` and `gc`; each fails with "not implemented".
- The pack, which declares the session runtime `room` with the command `gc-runtime-room`.
- The image `sarak.sh/mikluko/roomcity`, with `roomcity`, `gc` built from commit 294079d of gascity's `main`, `bd` 1.3.4, `dolt` 2.7.2, `git`, Claude Code 2.1.311 and goose 1.59.1.

[Unreleased]: https://sarak.sh/mikluko/roomcity/compare/v0.3.0...HEAD
[0.3.0]: https://sarak.sh/mikluko/roomcity/releases/tag/v0.3.0
[0.2.0]: https://sarak.sh/mikluko/roomcity/releases/tag/v0.2.0
[0.1.0]: https://sarak.sh/mikluko/roomcity/releases/tag/v0.1.0

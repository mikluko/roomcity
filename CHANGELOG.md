# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/2.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- The owner's message goes into the running turn at once where the agent takes a message into it, as Claude Code does through steering, and waits for the turn's end where it does not, as goose does: `Prompt.into_turn` in `host.proto` asks it for the owner's prompts, and the new call `HostService.Handled` tells the server whether the host steered a prompt into the turn, began a turn with it, or queued it. The server writes the prompt's Waiting and Delivered records from that report alone, so the room's ⏳ is on exactly the messages that wait.
- The city's space, named for the city, with a room for each always-on agent in it and the journal, which the city's own user writes: a notice with a link to the room when a session for a task starts, one with a link to its transcript when it ends, and one when an always-on agent's session begins anew. A session started for a task has a room of its own, named for the agent and the session and open to the space's members without an invitation; at the session's end the room leaves the space and the agent's user leaves the room. The `name` and `transcripts` keys of `[chat]` set the space's name and the transcripts' address.
- The host gives its agent the secrets the server mounts as files: each variable `ROOMCITY_SECRET_<NAME>=<path>` of its environment sets `NAME` for the agent to the content of that file, and a file that cannot be read ends the host, naming the variable and the path.
- The session's stream in an agent's room as decided: thoughts as one folded notice for each run, a tool call as a notice for its start and one in reply for its result, the plan as one notice edited in place at most every 1.2 s, the typing notice for the whole turn and off while a permission request waits, a permission request as a message with a reaction for each option, answered by the first owner's reaction, and gc's nudge as a notice.
- In an always-on agent's room, `!stop`, which cancels the turn, and `!help`; an unknown command is answered with the known ones. A message to a session that is not running is answered with a notice that it was not taken. The reaction ⏳ marks the owner's message while it waits for the turn's end.
- `roomcity host` drives an agent that is an ACP process of its own: when the command the server stages names a program other than `claude`, the host runs it in the working directory and speaks ACP on its standard streams, framing and reporting it as it does Claude Code. Tried on `goose acp` with `kimi-for-coding`.
- `roomcity server`: it opens the registry, reads `roomcity.toml` (`-config`, or `ROOMCITY_CONFIG`) and serves on three listeners: the network (`HostService`, `GcService`), a loopback port for the runtime alone (`server.runtime_listen`, 127.0.0.1:8082) and a loopback port for the transcripts page's proxy (`server.page_listen`, 127.0.0.1:8081). Each session is a Kubernetes Job in the server's namespace.
- `roomcity runtime`, also `gc-runtime-room`: gc's exec session runtime. It hands each operation to the server at `ROOMCITY_RUNTIME_URL`. `start`, `stop`, `is-running`, `process-alive`, `list-running`, `nudge` and the meta operations are carried out; the rest answer exit 2, as an operation that is not offered.
- The registry, in SQLite: sessions, one numbering of each session's frames, prompts and lifecycle, the queue of instructions that survives a restart, the marks gc keeps, and the bundle a start stages for the host.
- A Secret of `roomcity.toml` that has a `key` may name `env`, the variable the agent gets the file's content in. The Job then carries `ROOMCITY_SECRET_<VARIABLE>`, the path of the mounted file, and the value is never in the pod's spec.
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

[Unreleased]: https://sarak.sh/mikluko/roomcity/compare/v0.1.0...HEAD
[0.1.0]: https://sarak.sh/mikluko/roomcity/releases/tag/v0.1.0

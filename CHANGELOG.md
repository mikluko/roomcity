# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/2.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.9.0] - 2026-11-16

### Added

- An agent sends a file of its working directory to its room with the tool `attach` of the MCP server `room`, which the session host gives every agent that takes http MCP servers (Claude Code through claudeacp, goose): an image is shown in the conversation, any other file is a file the owner can download, and the agent is told why when the room refuses one (outside the working directory, too large, a kind the room does not take). The host answers the agent's request for leave to use the room's own tools itself, so that the owner is not asked. `[chat] max_file` (default 10 MiB) and `file_kinds` (default `image/*`, `text/*`, `application/pdf`, `application/json`) bound what the room takes.
- `HostService.Send` reaches the room: a file a session's agent sends goes through `Attach` of the chat side, and the chat side's refusals (`ErrFileTooLarge`, `ErrFileKind`, `ErrNoRoom`, `ErrChatDown`) reach the agent as `InvalidArgument`, `FailedPrecondition` and `Unavailable` with the text that names the bound. `[chat] max_file` is the one size for the server's own bound and the limit on the network listener, so a city that raises it is no longer refused at 10 MiB. A server with no `[matrix]` answers `Unavailable`.
- `chat.Driver.SendFile` and `(*chat.Service).Attach`, the room's end of an agent's `attach`: a file is uploaded to the homeserver's media repository as the agent's user and sent as an `m.image` (a PNG, JPEG or GIF whose size is read, with its width and height) or an `m.file`; an SVG is a file until a client is seen to draw one. The caption is the body and the name the `filename`; with no caption the name is the body. `Attach` refuses a file whose bytes are of another kind than it is called (a zip called `notes.txt`, an HTML page called a picture: the bytes are looked at, since the kind is the caller's word), a file over `[chat] max_file` (a size such as `"10MiB"`, default 10 MiB) or of a media type that `[chat] file_kinds` does not list (default `image/*`, `text/*`, `application/pdf`, `application/json`) with an error that says what the room takes: `chat.ErrFileTooLarge`, `chat.ErrFileKind`, `chat.ErrNoRoom`, `chat.ErrChatDown`. A binary kind that the sniffer does not know cannot be sent whatever `file_kinds` lists, since its bytes read as something else: a `.docx` is refused as "its bytes are application/zip".

### Changed

- The database is of schema version 4: a session's stream may hold a `File` event, a file the session sent to its room, which the transcript page and the raw stream show. A server of version 3 refuses a database of version 4 rather than fail on a transcript that holds one. The step from 3 changes only the version.
- A permission request that the host answered itself, as it does for the agent's leave to use the room's own tools (`_meta.roomcity.answeredBy` is `"host"` in the call), is not posted to the room as a question with reactions. The tool call it was for shows as any other.

## [0.8.0] - 2026-11-16

### Added

- `[chat] catch_up`, a duration such as `"1h"` (default one hour): how old an event may be and still be shown when a room is brought up to date after an outage. What is older is left out, and the room says so in one notice that links the session's transcript.

### Changed

- The database is of schema version 3. A database of version 2 is migrated when the server opens it, after the copy of it as it was (`roomcity.v2.<time>.db`): the `shown` cursor of each session moves to its last event. Version 2 had the column and nothing kept it, so it stood where the migration to version 2 had put it, and the first server to keep it would have shown again what the rooms had shown since.
- The chat side keeps, for each session, how far it has shown the stream, in the registry, and takes the stream up from there: a new session is shown from its first event (its role prompt, "From gc", included, which a subscription "from now" could miss), a session that was shown before a restart is not shown again, and what a session said while the homeserver could not be reached is shown when it is back, and is not counted as shown before. The record is the event before the oldest text still held back unsent (an answer's paragraph, thoughts, a plan's edit), so a restart in the middle of a paragraph shows that paragraph again and nothing before it; a message that does not reach the room because the homeserver is out of reach ends the reading, which is begun again from the record after a pause of five seconds, while one that the homeserver refuses for itself (a reaction it already holds) is logged and left out. A tool notice and a permission request are not sent before the plan's edit that waits. A room that stays out of reach is logged once when it goes, once a minute while it stays, and once when it is back.

## [0.7.0] - 2026-11-16

### Added

- `[server] purge_every`, a duration such as `"1m"` (default one hour): how often the transcripts past their `transcript_term` are removed. A short term and a short gap let a transcript be watched going.
- `!reset` in a room, from a holder of the owner's role, starts the session afresh: the server asks gc, with `gc session reset`, to stop it and start it again, and the room says when it has begun anew. `!help` lists it. A reset that cannot be asked, for a session that is not running or one that has no gc id recorded, is answered with the reason.

### Changed

- The variables by which gc knows who is calling (`GC_AGENT`, `GC_ALIAS`, `GC_SESSION_ID`, `GC_SESSION_NAME`, `GC_TEMPLATE`, `GC_SESSION_ORIGIN`, `GC_INSTANCE_TOKEN`, `GC_CONTINUATION_EPOCH`, `GC_RUNTIME_EPOCH`, `GC_PROVIDER`, `GC_STARTUP_PROMPT_DELIVERED`) are set from what the server kept of the session's start, in the session's marks, and no longer from what each request says, so that a session cannot speak to gc as another. The first three are the marks gc itself stamps in a session's life, and are read at each call. A session started before this release has no record and is still taken at its word, until it is started again.
- A session has one transcript for each of its starts. The page `/-/<session>/<n>` is the transcript of the nth start (and `/-/<session>/<n>/raw` its stream), `/-/<session>` the latest; the card says which start of how many, links the others, and says how the start ended (`stop`, `reset`, `failed` or `lost`), the chat room it was shown in and gc's id of the session. `Registry.Transcript` takes the start's number, 0 for the latest, and `session.Service` has `Starts` and `SetRoom`.
- The 90 days of `transcript_term` are counted for each start from its own end: a start that is not the session's latest is removed with its events once it ended that long ago, and the session whole once its last event is that old and it has ended. `Registry.Purge` returns how many starts it removed.
- The journal's notice that a session began anew links the transcript of the start that just closed. The chat side records the room of each start with `SetRoom`.

## [0.6.0] - 2026-11-16

### Added

- The registry keeps each start of a session: a number, when it began and ended, how it ended (`stop`; `reset`; `failed` for a start that did not come up; `lost` for a box found dead, or a pod lost without a stop), its chat room, the id of gc's session bead and the first and last event of it, so that one name has a transcript for each start (`Registry.Starts`, `SetRoom`). One numbering of events still runs across the starts.
- `Service.Reset` starts a session afresh by running `gc session reset` on the id of its bead in the city's directory; the stop and the start that follow end the start as a reset.
- `Info.Shown` and `Service.Shown`: a cursor, kept by the registry, of what the chat side has shown of a session, which advances and never goes back and which a start afresh leaves where it is. `Prompt` and `Lifecycle` events carry the time they were recorded, as `Frame` does.

### Changed

- The database is of schema version 2. A database of version 1 is migrated when the server opens it, after the server has written SQLite's own consistent copy of it as it was beside it (`roomcity.v1.<time>.db`: the name carries the time, so no copy that exists is in the way or replaced; the server logs its path and keeps it until an operator removes it; the README says how a rollback uses it), in one transaction: the sessions get the bead id and the cursor (at their last event, so that nothing already shown is shown again), and each Started or StartedAfresh event of the stream becomes a start, and the start that runs takes the id of gc's session bead from its session's marks, so that a session already running can be reset. A database of a later version is refused.
- `examples/minimal`: the Role gives the server only the rights its calls use (no watch, no right to make or delete a pod), and the StatefulSet has `fsGroup: 1000`, which a block volume needs for the pod's user to write it; a test holds its pack range to admit the newest release, as it does the image.
- `examples/minimal`'s README says more of what must be there before it (a homeserver in the cluster and how Tuwunel reads a registration, the pod's egress, a Dolt server that takes a connection without TLS), names each value to replace once with every place it stands in, says what a registration token may be and how to make one, and says how to look at the pod once it is applied. The README's note that no session had run end to end is gone.

### Fixed

- A message that is only a fold, the agent's thoughts and nothing else, no longer begins with an empty line in its plain text, which a client that does not show HTML shows open, nor with an empty paragraph in its HTML: the plain text begins with the fold's summary ("💭 Thinking"), and the HTML with the fold.
- The host's account of why an agent ended, in its error and its log, waits for the end of the agent's standard error, which can still be unread when the process has been seen to end: a failed start now and then lacked the agent's last words, 3 times in 300 on one processor. It waits at most a second, for an agent that left a process holding the pipe.

## [0.5.0] - 2026-11-16

### Fixed

- The gc the server runs for a session is told the server's own runtime address, `ROOMCITY_RUNTIME_URL`, from the listener the server opened, so `gc session kill`, `reset`, `peek`, `new` and a nudge to another session no longer fail with "ROOMCITY_RUNTIME_URL is not set". Of a session's variables, the server's gc takes only the eleven by which gc knows who is calling (`GC_AGENT`, `GC_ALIAS`, `GC_SESSION_ID`, `GC_SESSION_NAME`, `GC_TEMPLATE`, `GC_SESSION_ORIGIN`, `GC_INSTANCE_TOKEN`, `GC_CONTINUATION_EPOCH`, `GC_RUNTIME_EPOCH`, `GC_PROVIDER`, `GC_STARTUP_PROMPT_DELIVERED`); the rest, the runtime's address, the city, the store, the Dolt and the providers `GC_SESSION` and `GC_BEADS` among them, are the server's, where a session could replace any `GC_*` before.
- The journal's "began anew" notice, and the room's, for an always-on agent whose session had ended before the server started: the chat side did not follow an ended session, so the next start, which the registry writes to the session's stream before the watch tells anyone of it, was behind the subscription's cursor and was never shown. It follows every agent session from the first time it sees it, ended or not.

### Added

- The server logs one line for each gc command it runs for a session: the session, the command and subcommand, the exit code and the time taken, and never the arguments past them nor the output.

## [0.4.0] - 2026-11-16

### Added

- `examples/minimal`: the configuration of a city as a minimal example of a deployment, with the names of the deployment it was taken from replaced by placeholders: the city's repository (`city/`), the manifests of its place in a Kubernetes cluster (`k8s/`), the registration of the application service as a template, and a README that says what must be there before it, what to replace and in which order. The tests load its `roomcity.toml` with the server's own loader and parse the city's files, and the test workflow renders the manifests.

### Changed

- `BEADS_DOLT_PASSWORD`, the second name gc sets the Dolt password under, is held back from a session's pod by its full name, as `GC_DOLT_PASSWORD` is: bd in the pod reads its credentials from the file `BEADS_CREDENTIALS_FILE` names.
- The environment of a session's pod, `ROOMCITY_TOKEN` and the variables gc set for the agent, is in a Secret of the namespace that the server makes for the session, owned by its Job and removed with it, and no longer in the Job's spec, which whoever may read Jobs can read. The pod reads it with `envFrom`. The server's Role needs `create`, `update` and `delete` on `secrets` of the namespace (no `get`, no `list`: it can read none); without them a start fails with a message that says so, and the server does not fall back to a plain environment. The README says what the Secret holds and who can read it.

## [0.3.3] - 2026-11-16

### Fixed

- A session's pod gets the environment gc set for the agent, and not only the `GC_*` variables of it: a provider's `env` from `city.toml` (goose's `GOOSE_PROVIDER`, `GOOSE_MODEL`, `OPENAI_HOST`...) reaches the pod, so that `guest` no longer fails with "Configuration value not found: GOOSE_PROVIDER". What the pod has of its own is left out: `PATH`, `HOME`, `USER`, `LOGNAME`, `SHELL`, `PWD`, `OLDPWD`, `TMPDIR`, `HOSTNAME`, `TERM`, the locale (`LANG`, `LANGUAGE`, `LC_*`), `XDG_*` and `KUBERNETES_*`; the server's own `ROOMCITY_*` win; and `GC_DOLT_PASSWORD`, a secret gc sets, is not written into the Job's spec (bd reads its credentials from the file `BEADS_CREDENTIALS_FILE` names). The start logs, by name, what it left out.

### Changed

- A table's plain text, which notifications and plain clients show, keeps its rows and separators instead of its cells run together.
- A permission request no longer shows its command twice.
- A tool call that ended because its permission was withdrawn or refused is "❌ cancelled" or "❌ refused", without the agent's own sentence about it.
- A reaction on a permission request that is over, withdrawn or answered, is told once that the request is no longer open.

## [0.3.2] - 2026-11-16

### Fixed

- The chat side no longer loops on a room whose holder has joined. Inviting someone who was in a room already, as every holder is after the first start, was answered "cannot invite user that is joined or banned", which was taken as the room not being made, and tried again every five seconds with the rest of the room's setup never reached. The driver reads the room's membership first and invites only those who are not in it; a banned holder is left as they are, with a word in the log the first time.
- The host starts Claude Code from gc's command: it no longer passes on `--session-id` and `--resume` with their values, which claudeacp refuses and which made every `host` start end at once.
- A host's error carries the data of an ACP error, which its message leaves out (for goose's `session/new` "Internal error").

## [0.3.1] - 2026-11-16

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

[Unreleased]: https://sarak.sh/mikluko/roomcity/compare/v0.9.0...HEAD
[0.9.0]: https://sarak.sh/mikluko/roomcity/releases/tag/v0.9.0
[0.8.0]: https://sarak.sh/mikluko/roomcity/releases/tag/v0.8.0
[0.7.0]: https://sarak.sh/mikluko/roomcity/releases/tag/v0.7.0
[0.6.0]: https://sarak.sh/mikluko/roomcity/releases/tag/v0.6.0
[0.5.0]: https://sarak.sh/mikluko/roomcity/releases/tag/v0.5.0
[0.4.0]: https://sarak.sh/mikluko/roomcity/releases/tag/v0.4.0
[0.3.3]: https://sarak.sh/mikluko/roomcity/releases/tag/v0.3.3
[0.3.2]: https://sarak.sh/mikluko/roomcity/releases/tag/v0.3.2
[0.3.1]: https://sarak.sh/mikluko/roomcity/releases/tag/v0.3.1
[0.3.0]: https://sarak.sh/mikluko/roomcity/releases/tag/v0.3.0
[0.2.0]: https://sarak.sh/mikluko/roomcity/releases/tag/v0.2.0
[0.1.0]: https://sarak.sh/mikluko/roomcity/releases/tag/v0.1.0

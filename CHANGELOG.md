# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/2.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `roomcity host`, the program of a session's pod: it drives Claude Code through `claudeacp` v0.6.0 in its own process, takes the server's instructions (prompt, cancel, permission answer, stop) and reports every ACP message in both directions as a numbered frame, sending again what the server has not acknowledged. After a lost connection it attaches again, naming the last instruction it took. It reads `ROOMCITY_SESSION`, `ROOMCITY_TOKEN`, `ROOMCITY_SERVER` and `ROOMCITY_WORKDIR`, and `ROOMCITY_SETTINGS`, a settings file for claude, when set.
- The stand-in `gc`, `roomcity gc`: it sends its arguments, standard input, `GC_*` environment and directory to the server through `GcService` and ends with the exit code of the gc the server ran. It takes the session, its token and the server from `ROOMCITY_SESSION`, `ROOMCITY_TOKEN` and `ROOMCITY_SERVER`, and sends the token as a bearer token with the call.
- The server's side of `GcService`, package `internal/gchandler`: it runs the real `gc` in the city's directory with the session's `GC_*` variables, for a call that carries the session's own token.

## [0.1.0] - 2026-11-15

### Added

- The binary `roomcity` with the commands `runtime`, `server`, `host` and `gc`; each fails with "not implemented".
- The pack, which declares the session runtime `room` with the command `gc-runtime-room`.
- The image `sarak.sh/mikluko/roomcity`, with `roomcity`, `gc` built from commit 294079d of gascity's `main`, `bd` 1.3.4, `dolt` 2.7.2, `git`, Claude Code 2.1.311 and goose 1.59.1.

[Unreleased]: https://sarak.sh/mikluko/roomcity/compare/v0.1.0...HEAD
[0.1.0]: https://sarak.sh/mikluko/roomcity/releases/tag/v0.1.0

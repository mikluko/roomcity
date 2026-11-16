#!/bin/sh
# Runs the tools the roomcity image ships, inside the image, and fails on the
# first one that does not start. The Dockerfile's smoke stage runs it.
#
# Usage: sh smoke-image.sh
set -eux

ls -l /usr/local/bin/roomcity /usr/local/bin/gc-runtime-room /usr/local/lib/roomcity/bin/gc \
  /usr/local/bin/gc /usr/local/bin/bd /usr/local/bin/dolt /usr/local/bin/goose

test "$(id -u)" = 1000

roomcity -h

# The runtime command has no server address and the server no configuration
# here: each says so, with 1, never with gc's "unknown operation" 2.
expect_unset_runtime_url() {
  rc=0
  env -u ROOMCITY_RUNTIME_URL "$@" 2>/tmp/stderr || rc=$?
  cat /tmp/stderr
  test "$rc" = 1
  grep -q 'ROOMCITY_RUNTIME_URL is not set$' /tmp/stderr
}
expect_no_configuration() {
  rc=0
  "$@" 2>/tmp/stderr || rc=$?
  cat /tmp/stderr
  test "$rc" = 1
  grep -q '^roomcity server: ' /tmp/stderr
}
# The stand-in gc and the host have no session here: each says so, with 1.
expect_unset_session() {
  rc=0
  env -u ROOMCITY_SESSION "$@" 2>/tmp/stderr || rc=$?
  cat /tmp/stderr
  test "$rc" = 1
  grep -q 'ROOMCITY_SESSION is not set$' /tmp/stderr
}
expect_unset_runtime_url roomcity runtime protocol
expect_no_configuration roomcity server -config /nonexistent/roomcity.toml
expect_unset_session roomcity host
expect_unset_session roomcity gc version
expect_unset_runtime_url gc-runtime-room protocol
expect_unset_session /usr/local/lib/roomcity/bin/gc version

gc version
bd version
dolt version
git --version
claude --version
goose --version

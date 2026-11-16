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

# Each command exists and does nothing: it fails with 1, never with gc's
# "unknown operation" 2.
expect_not_implemented() {
  rc=0
  "$@" 2>/tmp/stderr || rc=$?
  cat /tmp/stderr
  test "$rc" = 1
  grep -q ': not implemented$' /tmp/stderr
}
# The stand-in gc has no session to speak for here: it says so, with 1.
expect_unset_session() {
  rc=0
  env -u ROOMCITY_SESSION "$@" 2>/tmp/stderr || rc=$?
  cat /tmp/stderr
  test "$rc" = 1
  grep -q 'ROOMCITY_SESSION is not set$' /tmp/stderr
}
expect_not_implemented roomcity runtime protocol
expect_not_implemented roomcity server
expect_not_implemented roomcity host
expect_unset_session roomcity gc version
expect_not_implemented gc-runtime-room protocol
expect_unset_session /usr/local/lib/roomcity/bin/gc version

gc version
bd version
dolt version
git --version
claude --version
goose --version

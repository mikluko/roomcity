#!/bin/sh
# Runs a local Tuwunel 2.0.0 for the tests of the chat driver, with a
# registration for a city "testcity" and one for an application that holds the
# owner's role, as the sandbox's are.
#
#   scripts/tuwunel-test.sh up            starts one and prints the variables to export
#   scripts/tuwunel-test.sh down <name>   removes the one named in ROOMCITY_TEST_TUWUNEL
#
# Each "up" has a container name and a port of its own, so two can run at once.
set -eu

image=ghcr.io/matrix-construct/tuwunel:v2.0.0
# The host's address as the container reaches it, and the port the test
# serves the city's transactions on.
port=$((20000 + $$ % 10000))
name=roomcity-tuwunel-$$
host_url=http://192.168.64.1:$port

case ${1:-} in
up)
  dir=$(mktemp -d)
  mkdir "$dir/appservices"
  cat >"$dir/tuwunel.toml" <<EOF
[global]
server_name = "test.local"
database_path = "/var/lib/tuwunel"
appservice_dir = "/etc/tuwunel/appservices"

[global.http]
ip_source = "rightmost_x_forwarded_for"

[global.http.listen]
address = ["0.0.0.0"]
port = 8008
EOF
  cat >"$dir/appservices/testcity.yaml" <<EOF
id: roomcity-testcity
url: $host_url
sender_localpart: testcity
as_token: testcity-as-token
hs_token: testcity-hs-token
rate_limited: false
namespaces:
  users:
    - exclusive: true
      regex: '^@testcity(-[a-z0-9._=-]+)?:test\\.local\$'
  aliases:
    - exclusive: true
      regex: '^#testcity-[a-z0-9._=-]+:test\\.local\$'
EOF
  cat >"$dir/appservices/testowner.yaml" <<EOF
id: testowner
url: null
sender_localpart: _testowner
as_token: testowner-as-token
hs_token: testowner-hs-token
rate_limited: false
namespaces:
  users:
    - exclusive: true
      regex: '^@_testowner(_[a-z0-9]+)?:test\\.local\$'
EOF
  container run -d --name "$name" -v "$dir:/etc/tuwunel:ro" "$image" -c /etc/tuwunel/tuwunel.toml >/dev/null
  ip=$(container inspect "$name" | jq -r '.[0].status.networks[0].ipv4Address | split("/")[0]')
  echo "export ROOMCITY_TEST_HOMESERVER=http://$ip:8008"
  echo "export ROOMCITY_TEST_LISTEN=0.0.0.0:$port"
  echo "export ROOMCITY_TEST_TUWUNEL=$name"
  ;;
down)
  container delete --force "${2:?usage: $0 down <name>}" >/dev/null
  ;;
*)
  echo "usage: $0 up | down <name>" >&2
  exit 2
  ;;
esac

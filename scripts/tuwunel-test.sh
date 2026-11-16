#!/bin/sh
# Runs a local Tuwunel 2.0.0 for the tests of the chat driver, with a
# registration for a city "testcity" and one for an application that holds the
# owner's role, as the sandbox's are.
#
#   scripts/tuwunel-test.sh up              starts one in a container, with Apple's
#                                           `container`, and prints the variables to export
#   scripts/tuwunel-test.sh down <name>     removes the container named in ROOMCITY_TEST_TUWUNEL
#   scripts/tuwunel-test.sh run <binary>    starts the binary as a process of this machine, as
#                                           CI does, and prints the variables as NAME=value
#                                           lines, for $GITHUB_ENV
#
# Each start has a name and ports of its own, so two can run at once.
set -eu

image=ghcr.io/matrix-construct/tuwunel:v2.0.0
as_port=$((20000 + $$ % 10000))

# configure writes the server's configuration and registrations into a
# directory: the database path, the address of the city's transactions.
configure() { # dir database_path appservice_dir city_url listen_port
  dir=$1
  mkdir -p "$dir/appservices"
  cat >"$dir/tuwunel.toml" <<EOF
[global]
server_name = "test.local"
database_path = "$2"
appservice_dir = "$3"

[global.http]
ip_source = "rightmost_x_forwarded_for"

[global.http.listen]
address = ["0.0.0.0"]
port = $5
EOF
  cat >"$dir/appservices/testcity.yaml" <<EOF
id: roomcity-testcity
url: $4
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
      regex: '^#testcity(-[a-z0-9._=-]+)?:test\\.local\$'
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
}

case ${1:-} in
up)
  name=roomcity-tuwunel-$$
  dir=$(mktemp -d)
  # The host's address as the container reaches it.
  configure "$dir" /var/lib/tuwunel /etc/tuwunel/appservices "http://192.168.64.1:$as_port" 8008
  container run -d --name "$name" -v "$dir:/etc/tuwunel:ro" "$image" -c /etc/tuwunel/tuwunel.toml >/dev/null
  ip=$(container inspect "$name" | jq -r '.[0].status.networks[0].ipv4Address | split("/")[0]')
  echo "export ROOMCITY_TEST_HOMESERVER=http://$ip:8008"
  echo "export ROOMCITY_TEST_LISTEN=0.0.0.0:$as_port"
  echo "export ROOMCITY_TEST_TUWUNEL=$name"
  ;;
down)
  container delete --force "${2:?usage: $0 down <name>}" >/dev/null
  ;;
run)
  bin=${2:?usage: $0 run <binary>}
  hs_port=$((30000 + $$ % 10000))
  dir=$(mktemp -d)
  configure "$dir" "$dir/db" "$dir/appservices" "http://127.0.0.1:$as_port" "$hs_port"
  nohup "$bin" -c "$dir/tuwunel.toml" >"$dir/tuwunel.log" 2>&1 &
  for _ in $(seq 1 60); do
    if curl -fsS "http://127.0.0.1:$hs_port/_matrix/client/versions" >/dev/null 2>&1; then
      echo "ROOMCITY_TEST_HOMESERVER=http://127.0.0.1:$hs_port"
      echo "ROOMCITY_TEST_LISTEN=127.0.0.1:$as_port"
      exit 0
    fi
    sleep 1
  done
  echo "Tuwunel did not answer; its log:" >&2
  cat "$dir/tuwunel.log" >&2
  exit 1
  ;;
*)
  echo "usage: $0 up | down <name> | run <binary>" >&2
  exit 2
  ;;
esac

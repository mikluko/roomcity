#!/bin/sh
# The command of the pod's gc container: brings the checkout of the city's
# repository to CITY_REVISION, or to its upstream's commit where that is unset,
# installs the city's packs, and runs the gc supervisor in the foreground. Each
# step is safe to repeat.
#
# From the environment: CITY_REPOSITORY, CITY_BRANCH, CITY_REVISION (optional),
# CITY_NAME, CITY_DOLT_HOST, CITY_DOLT_PORT, CITY_DOLT_USER and
# CITY_DOLT_DATABASE. The secrets forgejo and dolt are mounted under
# /run/roomcity/secrets.
set -eu

secrets=/run/roomcity/secrets
city=$HOME/city

# git of every process of the account: the city's identity, and Forgejo over
# https with the token of the user the repository's secret names. The helper
# only answers; a mounted secret is read-only, which "credential-store" is not.
git config --global user.name "$CITY_NAME"
git config --global user.email "$CITY_NAME@noreply.example.org"
git config --global credential.https://git.example.org.helper \
	"!f() { [ \"\$1\" = get ] || return 0; printf 'username=%s\\npassword=%s\\n' \"\$(cat $secrets/forgejo/username)\" \"\$(cat $secrets/forgejo/token)\"; }; f"
export GIT_TERMINAL_PROMPT=0

[ -d "$city/.git" ] || git clone -q -b "$CITY_BRANCH" "$CITY_REPOSITORY" "$city"
cd "$city"
git fetch -q
if [ -n "${CITY_REVISION:-}" ]; then
	git reset -q --hard "$CITY_REVISION"
else
	git reset -q --hard '@{upstream}'
fi

# bd and gc reach the shared Dolt with the password of bd's credentials file,
# a link to the pod's secret.
mkdir -p "$HOME/.config/beads"
ln -sfn "$secrets/dolt/credentials" "$HOME/.config/beads/credentials"

# The city's store is a database of the shared Dolt, in server mode. A new
# database has no tables, and gc's pin refuses it: bd makes the store, as the
# city's Dolt user, and writes the store's description, the .beads/metadata.json
# that the pin reads, which then stays on the volume. bd init reads the
# password from its environment, bd's other commands from the credentials file.
# The Dolt is asked first: a server that cannot be reached ends the script with
# nothing removed or made, a database that has a store is taken as it is (bd
# init adopts it, and only when the volume has no description of it), and only
# a database with no store is made one.
password=$(cat "$secrets/dolt/password")
tables=$(DOLT_CLI_PASSWORD=$password dolt --host "$CITY_DOLT_HOST" --port "$CITY_DOLT_PORT" --no-tls \
	--user "$CITY_DOLT_USER" --use-db "$CITY_DOLT_DATABASE" sql -r csv -q "show tables like 'issues'")
if ! printf '%s\n' "$tables" | grep -qx issues || [ ! -s "$city/.beads/metadata.json" ]; then
	rm -f "$city/.beads/metadata.json"
	BEADS_DOLT_PASSWORD=$password bd init --server --external --server-host "$CITY_DOLT_HOST" --server-port "$CITY_DOLT_PORT" \
		--server-user "$CITY_DOLT_USER" --database "$CITY_DOLT_DATABASE" -p "$CITY_NAME" \
		--non-interactive --skip-agents --skip-hooks
fi

gc import install

# The workspace's name follows the checkout's directory, city, unless the
# city's site binding names it: .gc/site.toml is written whole, as it holds
# nothing else here.
mkdir -p .gc
printf 'workspace_name = "%s"\n' "$CITY_NAME" >.gc/site.toml.new
mv .gc/site.toml.new .gc/site.toml

# The city's store is pinned to the shared Dolt; unpinned, gc init sets up a
# Dolt server of gc's own.
gc beads city use-external --host "$CITY_DOLT_HOST" --port "$CITY_DOLT_PORT" --user "$CITY_DOLT_USER"

# A city already initialized, as at every start after the first, exits 2.
rc=0
gc init --json --preserve-existing --no-start --skip-provider-readiness . || rc=$?
if [ "$rc" != 0 ] && [ "$rc" != 2 ]; then
	echo "start.sh: gc init failed with $rc" >&2
	exit 1
fi

# gc register would start a supervisor of its own; this one is the
# container's.
mkdir -p "$HOME/.gc"
printf '[[cities]]\npath = "%s"\nname = "%s"\n' "$city" "$CITY_NAME" >"$HOME/.gc/cities.toml"

exec gc supervisor run

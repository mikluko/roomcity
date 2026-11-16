#!/bin/sh
# Fetches Tuwunel 2.0.0, the homeserver the Matrix driver's tests run against,
# into the directory given, and prints the path of its binary. The download is
# checked against the checksum pinned here. The binary is statically linked,
# so it runs on any Linux of the machine's architecture.
#
#   scripts/fetch-tuwunel.sh <directory>
set -eu

dir=${1:?usage: $0 <directory>}
base=https://github.com/matrix-construct/tuwunel/releases/download/v2.0.0-assets/v2.0.0-assets-release-default

case $(uname -m) in
x86_64)
  asset=$base-x86_64-v1-linux-gnu-tuwunel.zst
  sum=1e4df9d39b6a62d4c98fd8732c57525b03e4c61654df1d2b7cfb61da9af97a0d
  ;;
aarch64 | arm64)
  asset=$base-aarch64-v8-linux-gnu-tuwunel.zst
  sum=4a2b885d492817bcee21fcc43f8053384d9e6a9af3245faac679fa9612e40444
  ;;
*)
  echo "no Tuwunel binary for $(uname -m)" >&2
  exit 1
  ;;
esac

mkdir -p "$dir"
curl -fsSL --retry 3 -o "$dir/tuwunel.zst" "$asset"
echo "$sum  $dir/tuwunel.zst" | sha256sum -c - >&2
zstd -d -q -f "$dir/tuwunel.zst" -o "$dir/tuwunel"
chmod +x "$dir/tuwunel"
echo "$dir/tuwunel"

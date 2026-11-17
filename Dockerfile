# The image of the runtime room: gc's supervisor, the runtime's server and
# every session's pod run from it.
#
# Build:  container build -t roomcity .

FROM --platform=$BUILDPLATFORM golang:1.27.4-trixie AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -o /out/bin/roomcity ./cmd/roomcity && \
    ln -s roomcity /out/bin/gc-runtime-room && \
    install -d /out/lib/roomcity/bin && \
    ln -s /usr/local/bin/roomcity /out/lib/roomcity/bin/gc

# gc has no release past 1.5.1 that carries what the runtime is built on, so
# it is built from a revision of its main branch. The fetch names the commit,
# so git checks the source against it.
FROM --platform=$BUILDPLATFORM golang:1.27.4-trixie AS gc
ARG TARGETOS TARGETARCH
# renovate: datasource=git-refs depName=https://github.com/gastownhall/gascity branch=main
ARG GC_REVISION=f715d7b92d4725dc2da77b0f3b3d1c98edb26ca6
WORKDIR /src
RUN git init -q && \
    git fetch -q --depth 1 https://github.com/gastownhall/gascity.git "$GC_REVISION" && \
    git checkout -q FETCH_HEAD
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -buildvcs=false \
      -ldflags "-s -w -X main.version=dev -X main.commit=$GC_REVISION -X main.date=$(git show -s --format=%cI HEAD)" \
      -o /out/bin/gc ./cmd/gc

# Release archives, each checked against the SHA-256 recorded here for its
# version and architecture.
FROM --platform=$BUILDPLATFORM golang:1.27.4-trixie AS fetch
ARG TARGETARCH
ARG BD_VERSION=1.3.4
ARG BD_SHA256_amd64=175c26904bde3ff9168d2282ff6b56f17b17f60c22fa5acb3dacc9fb3465303a
ARG BD_SHA256_arm64=f4ee6eb3ec79bb2d7eb9d717c73efbcb6fa763cc70f726b4d055b72afdd68ad5
ARG DOLT_VERSION=2.7.2
ARG DOLT_SHA256_amd64=72825a65d96bf0a0967ec408ad43bc21afc9b1c899b84cb84344a35f567fb11f
ARG DOLT_SHA256_arm64=5498898fdb4b93ce69deb9e32fb9e0fbaceb734657e6013afc0e914ae9c10afe
ARG GOOSE_VERSION=1.59.1
ARG GOOSE_SHA256_amd64=04c08900ab2ae78b8e2f556f1ed535a91b61c080a3ff62e9fd8b280cceb544ef
ARG GOOSE_SHA256_arm64=e3619b2ebf76dbbe8924561fd748220f289a34671e2757cda1cd833e44194f8e
WORKDIR /fetch
RUN set -eu; \
    fetch() { \
      curl -fsSL -o "$1" "$2"; \
      echo "$3  $1" | sha256sum -c -; \
    }; \
    eval "bd_sum=\$BD_SHA256_$TARGETARCH dolt_sum=\$DOLT_SHA256_$TARGETARCH goose_sum=\$GOOSE_SHA256_$TARGETARCH"; \
    case "$TARGETARCH" in amd64) goose_arch=x86_64 ;; arm64) goose_arch=aarch64 ;; esac; \
    fetch bd.tar.gz "https://github.com/gastownhall/beads/releases/download/v$BD_VERSION/beads_${BD_VERSION}_linux_$TARGETARCH.tar.gz" "$bd_sum"; \
    fetch dolt.tar.gz "https://github.com/dolthub/dolt/releases/download/v$DOLT_VERSION/dolt-linux-$TARGETARCH.tar.gz" "$dolt_sum"; \
    fetch goose.tar.gz "https://github.com/aaif-goose/goose/releases/download/v$GOOSE_VERSION/goose-$goose_arch-unknown-linux-gnu.tar.gz" "$goose_sum"; \
    install -d /out/bin; \
    tar --no-same-owner -xzf bd.tar.gz -C /out/bin bd; \
    tar --no-same-owner -xzf dolt.tar.gz --strip-components=2 -C /out/bin "dolt-linux-$TARGETARCH/bin/dolt"; \
    tar --no-same-owner -xzf goose.tar.gz -C /out/bin ./goose

FROM debian:trixie-20261104-slim AS roomcity

ARG CLAUDE_CODE_VERSION=2.1.311-1
ARG DEBIAN_FRONTEND=noninteractive

RUN --mount=type=cache,target=/var/lib/apt,sharing=locked \
    --mount=type=cache,target=/var/cache/apt,sharing=locked \
    apt-get update -yq && \
    apt-get install -y --no-install-recommends \
        ca-certificates curl dumb-init git jq lsof openssh-client procps util-linux

COPY apt/keyrings/ /etc/apt/keyrings/
COPY apt/sources.list.d/ /etc/apt/sources.list.d/

RUN --mount=type=cache,target=/var/lib/apt,sharing=locked \
    --mount=type=cache,target=/var/cache/apt,sharing=locked \
    apt-get update -yq && \
    apt-get install -y --no-install-recommends "claude-code=$CLAUDE_CODE_VERSION"

COPY --from=fetch /out/bin/ /usr/local/bin/
COPY --from=gc /out/bin/ /usr/local/bin/
COPY --from=build /out/bin/ /usr/local/bin/
# The stand-in gc has a directory of its own: a session's pod puts it first on
# PATH, and gc's own containers do not.
COPY --from=build /out/lib/ /usr/local/lib/

RUN groupadd --gid 1000 city && \
    useradd --uid 1000 --gid city --create-home --shell /bin/bash city
USER 1000:1000
WORKDIR /home/city

ENTRYPOINT ["/usr/bin/dumb-init", "--"]

# The image's tools run inside it: `--target smoke` builds this stage, in the
# builder, so no daemon has to load the image.
FROM roomcity AS smoke
RUN --mount=type=bind,source=scripts/smoke-image.sh,target=/smoke-image.sh \
    sh /smoke-image.sh

# The default target is the image itself, unchanged by the smoke stage.
FROM roomcity

# A minimal deployment

This is the configuration of a city that runs on the session runtime `room`:
the city's repository, the manifests of its place in a Kubernetes cluster, and
the registration the homeserver takes. It is the configuration of the city the
runtime is developed against, with the names of that deployment replaced by
placeholders of the documentation kind (`example.org`).

A city of this directory has three agents. `host` runs Claude Code and `guest`
runs goose on Kimi's coding API, each always on, with its own room in the city's
space on Matrix. `worker` is started for one task and has a room for the length
of it. The owner talks to the agents in their rooms and answers their permission
requests with a reaction.

| Directory | What it is |
|---|---|
| `city/` | The city's repository: `city.toml`, `pack.toml` and the agents. Push it to a Git server of your own. |
| `k8s/` | The manifests of the city's place in a cluster, as plain Kubernetes, with a `kustomization.yaml`. `k8s/files/roomcity.toml` is the server's configuration. |
| `registration/` | The registration of the application service, as a template. |

## What it deploys

One StatefulSet `roomcity` of one pod, in the namespace `roomcity-example`, with
three containers:

- `gc` brings the checkout of the city's repository to its upstream's commit,
  makes the city's store in its database, installs the city's packs and runs
  gc's supervisor (`k8s/files/start.sh`).
- `roomcity` is the runtime's server. It makes a Job for each session, is the
  application service of the homeserver and keeps the transcripts.
- `oauth2-proxy` is the only way to the transcripts page, which the server
  serves on a loopback port of its own.

Besides them: the ServiceAccount and the Role that lets the server create, get,
list and delete Jobs, list their pods and read their logs, and create, update
and delete Secrets, which hold each session's environment, but read none, in its
namespace and nowhere else (the Job controller makes the pods, and nothing
watches); the Service that sessions and the homeserver reach the server at, the
Service and the Ingress of the page, and a NetworkPolicy that lets only the
namespace's pods and the homeserver reach the server, and only the ingress
controller reach the proxy.

## What must be there before it

- A Kubernetes cluster with an ingress controller, and a node on which a
  volume of the class you choose can be written by user 1000.
- A Matrix homeserver in the same cluster, with a user of yours on it, the
  owner: the registration's `url` and `k8s/network-policy.yaml` name cluster
  addresses, the homeserver's namespace and its pods' labels. It must take an
  application service registration. Only Tuwunel 2.0.0 is tested. Tuwunel reads
  the registration files of the directory that its option `appservice_dir`
  names, at startup only, so it is restarted to read one.
- A Dolt SQL server the pod can reach and that takes a connection without TLS
  (`k8s/files/start.sh` runs `dolt --no-tls`), with a user for the city and an
  empty database of its own that the user has all rights on.
- An OIDC provider for the transcripts page, with a client whose callback is
  `https://city.example.org/oauth2/callback`, and a group `admin` that the
  owner is in.
- A Git server with the city's repository and a user that can read it.
- A Claude Code token (for `host` and `worker`) and an API key for Kimi's coding
  API (for `guest`).
- Egress from the pod: to the Git server, to `github.com` and `sarak.sh`
  (`gc import install` fetches the packs that `city/pack.toml` names from the
  two), to the Dolt server and the homeserver, to Claude Code's API and to
  Kimi's (`https://api.kimi.com/coding`, in `city/city.toml`). The nodes pull
  the image from `sarak.sh` and the proxy's from `quay.io`.

## What to replace

Each value is one row, with every place it stands in. Replace the longer names
first: `example.org` ends `city.example.org`, `id.example.org`,
`git.example.org` and `noreply.example.org`, so it comes last.

| Value | Stands in | Replace with |
|---|---|---|
| `https://git.example.org/example/city.git` | `k8s/statefulset.yaml` (`CITY_REPOSITORY`) | the city's repository |
| `city.example.org` | `k8s/ui.ingress.yaml` (twice), `k8s/statefulset.yaml` (`--redirect-url`), `k8s/files/roomcity.toml` (`[chat] transcripts`) | the address of the transcripts page |
| `id.example.org` | `k8s/statefulset.yaml` (`--oidc-issuer-url`) | the OIDC provider's issuer |
| `00000000-0000-0000-0000-000000000000` | `k8s/statefulset.yaml` (`OAUTH2_PROXY_CLIENT_ID`) | the client ID of the OIDC client |
| `git.example.org` | `k8s/files/start.sh` (the key `credential.https://git.example.org.helper`) | the host of the repository's URL |
| `noreply.example.org` | `k8s/files/start.sh` (the city's commit email) | the domain of the identity the city commits as |
| `example.org` | `k8s/files/roomcity.toml` (`[matrix] server_name`, and the ids of the holders), `registration/sandbox.yaml.template` (both regexes, where it is written `example\.org`: keep the backslash; and its comments) | the homeserver's server name, which is what follows the colon of its Matrix ids and need not be the domain of the other hosts |
| `@owner:example.org`, `@owner-app:example.org` | `k8s/files/roomcity.toml` (`[[chat.holders]]`) | the Matrix ids that hold the owner's role; remove the block of the second if you have none. An id of the form `@<city>:` or `@<city>-…:` is the application service's: the owner's is not one |
| `sandbox`, as the city's name | `k8s/statefulset.yaml` (`CITY_NAME`), `k8s/files/roomcity.toml` (`[chat] name`, `[matrix] city`), `registration/sandbox.yaml.template` (`id`, `sender_localpart`, both regexes and its comments), `city/pack.toml` (`[pack] name`), the three `prompt.template.md` of `city/agents`, and step 7 below | the city's name, the same in all of them: lower-case letters and digits, no dash (`roomcity server` refuses a `[matrix] city` that is not `[a-z0-9]+`). `[matrix] city` and the registration's `sender_localpart` must be the same |
| `roomcity-example` | `k8s/kustomization.yaml` (`namespace`), `k8s/namespace.yaml`, `k8s/files/roomcity.toml` (`[kube] namespace`, and `[server] url`, whose host is the Service `roomcity` in that namespace), `registration/sandbox.yaml.template` (`url`, the same address) | the namespace |
| `http://homeserver.matrix.svc.cluster.local:8008` | `k8s/files/roomcity.toml` (`[matrix] homeserver`) | the homeserver's address in the cluster |
| `matrix` and `app.kubernetes.io/name: homeserver`; `ingress` and `app.kubernetes.io/name: ingress-controller` | `k8s/network-policy.yaml` | the namespace and the pods' label of your homeserver, and of your ingress controller |
| `dolt.dolt.svc.cluster.local`, `3306` | `k8s/statefulset.yaml` (`CITY_DOLT_HOST`, `CITY_DOLT_PORT`), `city/city.toml` (`[dolt] host`, `port`), and the section name of the Secret `dolt`'s `credentials` | the Dolt server's host and port |
| `city`, as the Dolt user | `k8s/statefulset.yaml` (`CITY_DOLT_USER`) | the city's user on the Dolt server. Not the other `city`: `/home/city`, `$HOME/city`, `[server] data` and `city_dir`, and `scope = "city"` in `city/` are the image's user, its home and the checkout, and stay |
| `sandbox`, as the Dolt database | `k8s/statefulset.yaml` (`CITY_DOLT_DATABASE`) | the city's database on the Dolt server; it need not be the city's name |
| `ingressClassName: example` | `k8s/ui.ingress.yaml` | your ingress class; add the annotations your cluster needs for a certificate and a DNS record |
| `# storageClassName: standard` | `k8s/statefulset.yaml`, commented out | uncomment it with a class of your cluster, or leave it for the default |
| the image tag, `sarak.sh/mikluko/roomcity:<version>` | `k8s/statefulset.yaml` (both containers), `k8s/files/roomcity.toml` (`[kube] image`) | the release you run; the pod and `kube.image` must name the same image |
| the pack's range, `version` of `[imports.roomcity]` | `city/pack.toml` | the minor of the release you run: a caret below 1.0 holds the minor, and the pack and the image of one release are kept together |

## Where it departs from the deployment

The files are those of the deployment they were taken from, with the names
replaced by placeholders, and two departures. The pod's `securityContext` has
`fsGroup: 1000`, which a block volume needs for the pod's user to write it and
which the deployment's host-path volume ignores. And what is particular to that
cluster is left out: a node selector and tolerations, a storage class, and the
annotations that make a certificate and a DNS record from the Ingress.

## The Secrets

The manifests name six Secrets and hold none. Make them in the namespace before
the pod starts, with the keys below.

| Secret | Keys | What they hold |
|---|---|---|
| `forgejo` | `username`, `token` | a user of the Git server and its token, which read the city's repository |
| `dolt` | `password`, `credentials` | the city's Dolt user's password; and bd's credentials file, `[dolt.dolt.svc.cluster.local:3306]` then `password = <the password>` on the next line (the section is the host and port of `[dolt]` in `city/city.toml`) |
| `matrix` | `as-token`, `hs-token` | the two tokens of the registration |
| `claude` | `token` | a Claude Code OAuth token |
| `kimi` | `api-key` | the API key of Kimi's coding API |
| `oauth2-proxy` | `client-secret`, `cookie-secret` | the OIDC client's secret, and 32 random letters and digits |

For example: `kubectl -n roomcity-example create secret generic kimi --from-file=api-key=./kimi-key`.

The two tokens of the registration are any text that is not empty; `roomcity
server` cuts the whitespace around each. `openssl rand -hex 32` makes one.

## The order

1. Make the city's repository on your Git server, with the files of `city/` at
   its root and `main` as its branch, and the user that reads it.
2. Make the Dolt user and the database. Nothing else is needed there: the pod
   makes the city's store in it at its first start.
3. Make the OIDC client and note its ID and secret.
4. Replace the placeholders. Make the namespace (`kubectl apply -f k8s/namespace.yaml`)
   and the Secrets.
5. Fill `registration/sandbox.yaml.template` in (the same two tokens as the
   Secret `matrix`), give it to the homeserver (Tuwunel: in the directory its
   `appservice_dir` names) and restart the homeserver so that it reads the
   registration.
6. `kubectl apply -k k8s`. The pod starts: `gc` makes the store and the
   supervisor starts the city's always-on agents, which makes the server start a
   Job for each.
7. The city's space and the rooms of `host` and `guest` appear on the homeserver;
   the owner is invited. Join `#sandbox-host:example.org` and write to it.

To see what the pod does: `kubectl -n roomcity-example logs roomcity-0 -c gc`
is the steps of `start.sh` and the supervisor, and `-c roomcity` the server. A
session's Job and pod are in `kubectl -n roomcity-example get jobs,pods`.

The first start of a session takes several seconds, since a pod has to start.
If `gc` has been refused a start several times in a row it holds the next one
back for a time; its sessions show `start-pending`.

## What it checks

The test workflow checks that `k8s` renders (`kustomize build examples/minimal/k8s`)
and that `k8s/files/roomcity.toml`, `city/city.toml` and `city/pack.toml` parse:
the first with the server's own loader. A test also holds the image of the
example to the newest release in `CHANGELOG.md`, so that a release's pull
request moves it.

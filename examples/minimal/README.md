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

Besides them: the ServiceAccount and the Role that lets the server create and
delete Jobs and pods in its namespace and nowhere else, the Service that
sessions and the homeserver reach the server at, the Service and the Ingress of
the page, and a NetworkPolicy that lets only the namespace's pods and the
homeserver reach the server, and only the ingress controller reach the proxy.

## What must be there before it

- A Kubernetes cluster with an ingress controller, and a node on which a
  volume of the class you choose can be written by user 1000.
- A Matrix homeserver that takes an application service registration and
  restarts to read it (Tuwunel does; its registrations are read at startup), and
  a user of yours on it, the owner.
- A Dolt SQL server the pod can reach, with a user for the city and an empty
  database of its own that the user has all rights on.
- An OIDC provider for the transcripts page, with a client whose callback is
  `https://city.example.org/oauth2/callback`, and a group `admin` that the
  owner is in.
- A Git server with the city's repository and a user that can read it.
- A Claude Code token (for `host` and `worker`) and an API key for Kimi's coding
  API (for `guest`).

## What to replace

| Placeholder | Where | Replace with |
|---|---|---|
| `example.org` | `k8s/files/roomcity.toml`, `registration/`, `k8s/statefulset.yaml` | the homeserver's server name and your domain |
| `city.example.org` | `k8s/ui.ingress.yaml`, `k8s/statefulset.yaml`, `k8s/files/roomcity.toml` | the address of the transcripts page |
| `id.example.org` | `k8s/statefulset.yaml` | the OIDC provider's issuer |
| `00000000-0000-0000-0000-000000000000` | `k8s/statefulset.yaml` | the client ID of the OIDC client |
| `https://git.example.org/example/city.git` | `k8s/statefulset.yaml` | the city's repository |
| `git.example.org`, `noreply.example.org` | `k8s/files/start.sh` | the Git server, and the domain of the identity the city commits as |
| `@owner:example.org`, `@owner-app:example.org` | `k8s/files/roomcity.toml` | the Matrix ids that hold the owner's role; remove the second if you have none |
| `homeserver.matrix.svc.cluster.local` | `k8s/files/roomcity.toml` | the homeserver's address in the cluster |
| `dolt.dolt.svc.cluster.local`, `city`, `sandbox` | `k8s/statefulset.yaml`, `city/city.toml` | the Dolt server, the city's user and database on it (the host in `city/city.toml` is the same as `CITY_DOLT_HOST`) |
| `roomcity-example` | `k8s/`, `registration/` | the namespace |
| `ingressClassName: example` | `k8s/ui.ingress.yaml` | your ingress class; add the annotations your cluster needs for a certificate and a DNS record |
| the labels and namespace of the homeserver and of the ingress controller | `k8s/network-policy.yaml` | those of your cluster |
| `storageClassName` | `k8s/statefulset.yaml` | a class of your cluster, or leave it out for the default |
| the image tag, `sarak.sh/mikluko/roomcity:<version>` | `k8s/statefulset.yaml`, `k8s/files/roomcity.toml` | the release you run; the pod and `kube.image` must name the same |

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

## The order

1. Make the city's repository from `city/` on your Git server, with `main` as
   its branch, and the user that reads it.
2. Make the Dolt user and the database. Nothing else is needed there: the pod
   makes the city's store in it at its first start.
3. Make the OIDC client and note its ID and secret.
4. Replace the placeholders. Make the namespace (`kubectl apply -f k8s/namespace.yaml`)
   and the Secrets.
5. Fill `registration/sandbox.yaml.template` in (the same two tokens as the
   Secret `matrix`), give it to the homeserver and restart the homeserver so
   that it reads the registration.
6. `kubectl apply -k k8s`. The pod starts: `gc` makes the store and the
   supervisor starts the city's always-on agents, which makes the server start a
   Job for each.
7. The city's space and the rooms of `host` and `guest` appear on the homeserver;
   the owner is invited. Join `#sandbox-host:example.org` and write to it.

The first start of a session takes several seconds, since a pod has to start.
If `gc` has been refused a start several times in a row it holds the next one
back for a time; its sessions show `start-pending`.

## What it checks

The test workflow checks that `k8s` renders (`kustomize build examples/minimal/k8s`)
and that `k8s/files/roomcity.toml`, `city/city.toml` and `city/pack.toml` parse:
the first with the server's own loader. A test also holds the image of the
example to the newest release in `CHANGELOG.md`, so that a release's pull
request moves it.

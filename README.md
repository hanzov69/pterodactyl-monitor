# pterodactyl-monitor

Small Go service that watches a Pterodactyl install, emails you when something
goes down (and when it recovers), and serves a status page with rolling uptime.

Out of the box it monitors the panel and its nodes. Monitoring the individual
servers on the panel is an [optional add-on](#optional-server-monitoring) that
stays completely out of the way until you give it a key.

## What it checks

Every `CHECK_INTERVAL` (default 60s):

| Target | Check | Up when |
|---|---|---|
| Panel (public) | `GET PANEL_PUBLIC_URL` | HTTP status < 400 |
| Panel (LAN) | `GET PANEL_LAN_URL` | HTTP status < 400 |
| Panel API | `GET /api/application/nodes` with the API key | HTTP 200 |
| Each node | `GET <scheme>://<fqdn>:<daemon port>/api/system` (Wings) | Wings answers (a 401 without its token still proves it is alive) |

Nodes are discovered from the panel API each cycle, so added/removed nodes are
picked up automatically. Nodes in maintenance mode are shown but not alerted on.
If the API is unreachable, the last known node list keeps being checked.

A target is **DOWN** after `FAIL_THRESHOLD` (default 3) consecutive failures.
One email is sent on DOWN and one on recovery; changes in the same cycle are
combined into a single message, and failed sends are retried each cycle.

History is kept for 30 days in `/data/state.json` (5-minute buckets).

## Deploy

Kubernetes manifests are a kustomize base (`k8s/base`) plus an example overlay
(`k8s/example`). Your real settings go in `k8s/local/`, which is git-ignored.

```sh
# 1. Create your overlay
cp -r k8s/example k8s/local
$EDITOR k8s/local/config.env          # panel URLs, SMTP host/from/to
$EDITOR k8s/local/secret.env          # API key, SMTP credentials
$EDITOR k8s/local/kustomization.yaml  # LAN CIDR for the dashboard, storage class, image

# 2. Apply
kubectl apply -k k8s/local
kubectl -n pterodactyl-monitor get svc          # EXTERNAL-IP is the dashboard address

# 3. Verify email
kubectl -n pterodactyl-monitor exec deploy/pterodactyl-monitor -- /pterodactyl-monitor -test-email
```

The API key is an **Application API** key (Admin → Application API) with
`Nodes: Read`. Re-running `kubectl apply -k k8s/local` after editing either env
file restarts the pod with the new settings.

To build your own image instead of using `ghcr.io/hanzov69/pterodactyl-monitor`:

```sh
docker buildx build --platform linux/amd64 -t <registry>/<you>/pterodactyl-monitor:latest --push .
```

and point the overlay at it with the `images:` block in `kustomization.yaml`.

## LAN-only access

The base Service is `ClusterIP`. The example overlay turns it into a
`LoadBalancer` (MetalLB, kube-vip, ServiceLB, ...) on a private address and
sets `loadBalancerSourceRanges` to your LAN's CIDR, so requests from anywhere
else are rejected. Don't put it behind an internet-facing Ingress.

## Configuration

| Variable | Default | |
|---|---|---|
| `PANEL_PUBLIC_URL`, `PANEL_LAN_URL` | – | At least one required |
| `PANEL_API_URL` | LAN URL | Where to reach the application API |
| `PANEL_API_KEY` | – | Application API key (`ptla_`). Without it, nodes aren't monitored |
| `LAN_TLS_SKIP_VERIFY`, `NODE_TLS_SKIP_VERIFY` | `false` | For mismatched / self-signed certs |
| `CHECK_INTERVAL`, `CHECK_TIMEOUT`, `FAIL_THRESHOLD` | `60s`, `10s`, `3` | |
| `SMTP_HOST`, `SMTP_PORT`, `SMTP_TLS` | –, `587`, `starttls` | `SMTP_TLS`: `starttls`, `tls`, `none` |
| `SMTP_USERNAME`, `SMTP_PASSWORD`, `SMTP_FROM`, `SMTP_TO` | – | `SMTP_TO` may be comma-separated |
| `DASHBOARD_URL` | – | Link appended to alert emails |
| `DATA_DIR`, `LISTEN_ADDR` | `/data`, `:8080` | |

## Optional: server monitoring

| Variable | Default | |
|---|---|---|
| `PANEL_CLIENT_KEY` | – | Account API key (`ptlc_`). Setting it turns server monitoring on |
| `SERVER_CHECKS` | – | Per-server service checks (JSON, see [Setting checks yourself](#setting-checks-yourself)) |
| `START_GRACE` | `15m` | How long a server may be starting/stopping before it counts as failing |

Everything in this section is off unless `PANEL_CLIENT_KEY` is set. Without
it the monitor makes no server-related API calls and the dashboard shows only
the panel and nodes.

Set `PANEL_CLIENT_KEY` to an **account** API key (`ptlc_...`, Account → API
Credentials) and the dashboard gains a **Servers** tab listing every server
with its own uptime history. An admin's key sees every server on the panel; a
regular user's key sees the servers that user owns or is a subuser of. The
monitor only reads with it, but the key itself can do whatever its account
can, so treat it accordingly.

Each server's power state (running / starting / stopping / offline) comes from
the panel. A running server can also get a service check:

| Check | What it does | Chosen automatically for |
|---|---|---|
| `a2s` | Steam server query (what the in-game server browser sends); reports players and map | Rust, Valheim, Enshrouded, Project Zomboid, Source-engine servers |
| `minecraft` | Java Edition status ping; reports players and version | Minecraft Java servers and proxies |
| `palworld` | Logs in to RCON with the admin password set in the panel; reports players and version | Palworld |
| `raknet` | RakNet ping on the game port | Don't Starve Together |
| `http` | `GET` a URL, up when status < 400 | nginx / Apache / Caddy style web servers, on the primary allocation |
| `tcp` | Connects to a port | - |
| `none` | Power state only | Everything else |

### Automatic checks

There is nothing to configure for the server types in the last column: once
`PANEL_CLIENT_KEY` is set, each server is matched by its egg name, Docker
image and startup command, and the check uses the ports the egg already
defines.

| Server type | Check | Where it connects | What has to be true |
|---|---|---|---|
| Rust | `a2s` | The egg's `QUERY_PORT` variable (else the primary port), UDP | - |
| Valheim | `a2s` | Primary port + 1, UDP | Crossplay off. Crossplay servers go through PlayFab and answer nothing locally, so they get no check |
| Enshrouded | `a2s` | The egg's `QUERY_PORT` variable (else primary port + 1), UDP | - |
| Project Zomboid | `a2s` | Primary port, UDP | - |
| Source engine (`srcds`) | `a2s` | Primary port, UDP | - |
| Palworld | `palworld` | The egg's `RCON_PORT` variable, TCP | RCON enabled on the server, and the egg's `ADMIN_PASSWORD` variable matching it |
| Minecraft Java | `minecraft` | Primary port, TCP | `enable-status` not turned off in `server.properties` |
| Don't Starve Together | `raknet` | Primary port, UDP | - |
| nginx / Apache / Caddy | `http` | `http://<primary allocation>/` | - |

Two things apply to all of them:

- **The monitor must be able to reach the port.** Checks go to the
  allocation's IP address; if that is `0.0.0.0` or loopback, to its alias, and
  failing that to the node's FQDN. The port has to be one of the server's
  allocations (Wings only exposes allocated ports), and open to wherever the
  monitor runs - UDP for `a2s` and `raknet`.
- **The egg variables must be visible to the key's account.** The ports and
  the Palworld password are read from the server's startup variables through
  the client API, which only returns variables marked as viewable by users.

The Palworld check logs in to RCON and runs the read-only `ShowPlayers` and
`Info` commands. The password is read from the panel on every cycle and kept
in memory only: it is never written to the state file, the dashboard or the
logs, and it cannot be supplied through `SERVER_CHECKS`. If the password
variable isn't available the check falls back to testing that the RCON port
is open.

The Enshrouded and Project Zomboid rules follow those games' documented query
ports but have not been exercised against live servers.

### Setting checks yourself

Use `SERVER_CHECKS` when a server isn't recognised, is recognised wrongly, or
listens somewhere unusual. It is a JSON object (on one line) keyed by server
name (case-insensitive), short id or UUID; an entry replaces whatever would
have been detected for that server.

| Field | Used by | Default |
|---|---|---|
| `type` | all | required: `a2s`, `minecraft`, `palworld`, `raknet`, `http`, `tcp` or `none` |
| `url` | `http` | `http://<host>:<port>/` |
| `host` | everything except `http` with a `url` | The primary allocation's address (see above) |
| `port` | everything except `http` with a `url` | The primary allocation's port; for `palworld`, the egg's `RCON_PORT` |

```
# A Steam-query game that isn't detected, with its query port on a second allocation
SERVER_CHECKS={"My ARK Server":{"type":"a2s","port":27015}}

# Several at once: a site checked at its public URL, a Palworld server whose egg
# isn't recognised, a Valheim on a non-standard query port, and a bot with no check
SERVER_CHECKS={"My Website":{"type":"http","url":"https://www.example.com"},"1a2b3c4d":{"type":"palworld"},"Valheim":{"type":"a2s","port":2500},"Some Bot":{"type":"none"}}
```

The dashboard shows which check each server ended up with (for example
`a2s://10.0.0.5:28016`) and, when it fails, why (`running, but no answer to
server query: timed out`), which is the quickest way to see whether a port or
host needs overriding.

### How a server's status is decided

- **offline**, or running with a failing service check, counts as a failed check.
- **starting / stopping** is not a failure until it has lasted longer than
  `START_GRACE` (default 15m), so restarts don't alert but a server stuck
  booting does.
- A server that is already off when the monitor first sees it is shown as
  offline without being reported; it is only treated as an outage once it has
  been seen up. The same applies to a service check that has never passed, so
  a wrongly detected check shows as down but never emails you (set it to
  `none` or the right type in `SERVER_CHECKS`).
- Suspended and installing servers are shown but never alerted on.
- If the panel can't report a server's state (node unreachable), the service
  check decides on its own; without one the server is shown as unknown.

Each server row has an **alerts** switch. Switching it off stops emails for
that server (it is still checked, charted and listed under recent events) and
keeps it out of the headline status. The dashboard has no login: anyone who
can reach it can flip these switches, so keep it on a trusted network.

## Run locally

```sh
PANEL_LAN_URL=http://192.168.1.10 PANEL_API_KEY=ptla_... DATA_DIR=./data go run .
# http://localhost:8080
```

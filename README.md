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
| `SERVER_CHECKS` | – | Per-server service checks (JSON, see below) |
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
| `a2s` | Steam server query (what the in-game server browser sends); reports players and map | Rust (on its query port), Source-engine servers |
| `http` | `GET` a URL, up when status < 400 | nginx / Apache / Caddy style web servers, on the primary allocation |
| `tcp` | Connects to a port | - |
| `none` | Power state only | Everything else |

Override or add checks per server with `SERVER_CHECKS`, a JSON object keyed by
server name, short id or UUID:

```
SERVER_CHECKS={"My Website":{"type":"http","url":"https://www.example.com"},"1a2b3c4d":{"type":"tcp","port":25565},"Some Bot":{"type":"none"}}
```

`host` and `port` default to the server's primary allocation.

How a server's status is decided:

- **offline**, or running with a failing service check, counts as a failed check.
- **starting / stopping** is not a failure until it has lasted longer than
  `START_GRACE` (default 15m), so restarts don't alert but a server stuck
  booting does.
- A server that is already off when the monitor first sees it is shown as
  offline without being reported; it is only treated as an outage once it has
  been seen running.
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

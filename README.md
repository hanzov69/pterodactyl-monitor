# pterodactyl-monitor

Small Go service that watches a Pterodactyl install, emails you when something
goes down (and when it recovers), and serves a status page with rolling uptime.

## What it checks

Every `CHECK_INTERVAL` (default 60s):

| Target | Check | Up when |
|---|---|---|
| Panel (public) | `GET PANEL_PUBLIC_URL` | HTTP status < 400 |
| Panel (LAN) | `GET PANEL_LAN_URL` | HTTP status < 400 |
| Panel API | `GET /api/application/nodes` with the API key | HTTP 200 |
| Each node | `GET <scheme>://<fqdn>:<daemon port>/api/system` (Wings) | Wings answers (200, or 401/403 if the daemon token couldn't be read) |

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
| `PANEL_API_KEY` | – | Without it, nodes aren't monitored |
| `LAN_TLS_SKIP_VERIFY`, `NODE_TLS_SKIP_VERIFY` | `false` | For mismatched / self-signed certs |
| `CHECK_INTERVAL`, `CHECK_TIMEOUT`, `FAIL_THRESHOLD` | `60s`, `10s`, `3` | |
| `SMTP_HOST`, `SMTP_PORT`, `SMTP_TLS` | –, `587`, `starttls` | `SMTP_TLS`: `starttls`, `tls`, `none` |
| `SMTP_USERNAME`, `SMTP_PASSWORD`, `SMTP_FROM`, `SMTP_TO` | – | `SMTP_TO` may be comma-separated |
| `DASHBOARD_URL` | – | Link appended to alert emails |
| `DATA_DIR`, `LISTEN_ADDR` | `/data`, `:8080` | |

## Run locally

```sh
PANEL_LAN_URL=http://192.168.1.10 PANEL_API_KEY=ptla_... DATA_DIR=./data go run .
# http://localhost:8080
```

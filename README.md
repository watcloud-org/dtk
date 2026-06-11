# dtk — Docker Time Keeper

**dtk** is a lightweight reverse proxy that sits between your [Caddy](https://caddyserver.com) reverse proxy and your Docker-hosted services. It automatically **pauses** idle container groups after a configurable timeout, and **unpauses** them on demand when a request arrives — saving CPU and reducing noise while keeping services instantly reachable.

---

## How it works

```
Internet → Caddy (TLS) → dtk :8080 → target container
                              │
                              └── Docker socket
                                  (pause / unpause)
```

1. Caddy terminates TLS and forwards requests to dtk using `reverse_proxy dtk:8080`.
2. dtk routes by the HTTP **Host header** to find the matching group.
3. If the group is **running** → forward immediately (idle timer is reset).
4. If the group is **paused**:
   - **Browser** (accepts `text/html`) → return a loading page with a 3-second meta-refresh. The page keeps refreshing until the service is ready.
   - **API client** → hold the connection open, unpause the containers, wait `wake_delay`, then forward the request transparently.
5. After `idle_timeout` of no requests, dtk pauses all containers in the group.

---

## Quick start

### 1. Create a config file

```yaml
# /etc/dtk/config.yaml
listen: ":8080"

groups:
  - name: service
    host: service.example.com
    containers:
      - service_app
      - service_redis
    target: http://service_app:9999
    idle_timeout: 1h
    wake_delay: 5s
```

### 2. Add dtk to your `docker-compose.yml`

```yaml
services:
  dtk:
    image: ylecuyer/dtk:latest
    restart: unless-stopped
    volumes:
      - ./dtk-config.yaml:/etc/dtk/config.yaml:ro
      - /var/run/docker.sock:/var/run/docker.sock:ro
    networks:
      - proxy
      - services
```

> **Security note:** mounting the Docker socket gives dtk full control over the Docker daemon. Run dtk on a trusted host and restrict access accordingly. Consider using a Docker socket proxy (e.g. [Tecnativa/docker-socket-proxy](https://github.com/Tecnativa/docker-socket-proxy)) to limit the allowed API calls to `GET /containers/*`, `POST /containers/*/pause`, and `POST /containers/*/unpause`.

### 3. Configure Caddy

```caddy
service.example.com {
    reverse_proxy dtk:8080
}
```

### 4. Start

```sh
docker compose up -d
```

---

## Configuration reference

### Top-level

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `listen` | string | `":8080"` | Address dtk listens on |
| `groups` | list | — | One entry per service group (required) |

### `groups[]`

| Key | Type | Required | Default | Description |
|-----|------|----------|---------|-------------|
| `name` | string | ✓ | — | Human-readable name (used in logs and dashboard) |
| `host` | string | ✓ | — | HTTP Host header value that routes to this group |
| `containers` | list of strings | ✓ | — | Docker container names to pause/unpause together |
| `target` | string | ✓ | — | Upstream URL to forward requests to (e.g. `http://service_app:9999`) |
| `idle_timeout` | duration | | `30m` | Pause the group after this period of inactivity |
| `wake_delay` | duration | | `3s` | Wait this long after unpausing before forwarding |

Duration values use Go syntax: `30s`, `5m`, `1h30m`, etc.

---

## Dashboard

A live status dashboard is available at `/_dtk/status` (on any host):

```
http://localhost:8080/_dtk/status
```

It shows each group's current state, containers, last request time, and idle timeout. The page auto-refreshes every 5 seconds.

### Restart a group

From the dashboard, each group has a **Restart** button. Clicking it restarts every container configured in that group's `containers` list for the selected host.

This uses an internal endpoint:

```http
POST /_dtk/restart
Content-Type: application/x-www-form-urlencoded

host=service.example.com
```

Behavior:

- dtk sets the group state to `waking`.
- dtk restarts all containers in the group.
- dtk waits `wake_delay` before marking the group `running` again.
- on success, the endpoint redirects to `/_dtk/status` (`303 See Other`).

Error responses:

- `400` when `host` is missing
- `404` when the host does not match any configured group
- `405` for non-`POST` requests
- `504` if the request context is canceled or times out while waiting
- `500` for other restart failures

**States:**

| State | Meaning |
|-------|---------|
| `paused` | All containers are paused — no CPU usage |
| `waking` | Containers are being unpaused; requests are queued |
| `running` | Containers are running; requests are forwarded |
| `pausing` | Idle timer fired; containers are being paused |

---

## CLI flags

```
dtk [flags]

  -config string
        Path to config file (default "/etc/dtk/config.yaml")
  -docker-socket string
        Path to Docker socket (default "/var/run/docker.sock")
  -log-level string
        Log level: debug, info, warn, error (default "info")
```

---

## Building from source

```sh
# Requires Go 1.24+
git clone https://github.com/ylecuyer/dtk
cd dtk
go build -o dtk .
```

### Docker image

```sh
docker build -t dtk .
```

## Testing

```sh
go test -v ./...
```

### Coverage report

```sh
./scripts/coverage.sh
```

This generates:

- `coverage.out` (machine-readable profile)
- `coverage.html` (human-readable report)

Open `coverage.html` in a browser to inspect per-file and per-function coverage.

### Release to Docker Hub

```sh
# Login once per machine/session
docker login

# Push ylecuyer/dtk:latest
./scripts/release.sh

# Push an explicit version (also updates :latest)
./scripts/release.sh --tag v1.2.0

# Push to a different namespace/repository
./scripts/release.sh --repo myuser/dtk --tag v1.2.0
```

The multi-stage build produces a small Alpine-based image (~10 MB).

---

## Design notes

- **No external dependencies** except `gopkg.in/yaml.v3`. The Docker daemon is contacted directly over its REST API via the Unix socket using the Go standard library HTTP client.
- **Concurrency**: multiple simultaneous requests for a paused group share a single wake operation. The first request triggers the unpause; all others wait on a shared channel and are unblocked together when the group is ready.
- **No polling**: idle timeout is implemented with `time.AfterFunc` — no background goroutine loops.
- **Graceful shutdown**: SIGINT/SIGTERM causes dtk to pause all running groups before exiting.
- **WriteTimeout disabled**: the HTTP server has no write timeout so that connections can be held while containers wake up.

---

## License

MIT

# Docker Deployment

Run LLM API Proxy in production with Docker Compose.

## Quick Start

### 1. Create config

```bash
mkdir -p data
cp config.example.yaml data/config.yaml
# Edit data/config.yaml with your API keys
```

### 2. Create docker-compose.yml

```bash
cp docker-compose.example.yml docker-compose.yml
```

### 3. Start

```bash
docker compose up -d
```

The proxy is available at `http://localhost:8000`.

## docker-compose.yml Reference

```yaml
services:
  llmapiproxy:
    image: ghcr.io/vleeuwenmenno/llmapiproxy:latest
    ports:
      - "8000:8000" # API and web UI
      - "1455:1455" # Optional OAuth redirect server
    volumes:
      - ./data:/app/data # Persistent data
    restart: unless-stopped
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:8080/health"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 5s
```

### Port Mappings

| Port   | Purpose                                                        |
| ------ | -------------------------------------------------------------- |
| `8000` | API endpoint and web UI                                        |
| `1455` | Optional OAuth redirect server (for browser-based OAuth flows) |

### Volume Mounts

Mount `./data` to `/app/data` to persist:

| File/Directory     | Description                    |
| ------------------ | ------------------------------ |
| `data/config.yaml` | Configuration file             |
| `data/stats.db`    | Request statistics (SQLite)    |
| `data/chat.db`     | Chat sessions (SQLite)         |
| `data/users.db`    | Web UI users (SQLite)          |
| `data/tokens/`     | OAuth tokens (JSON files)      |
| `data/caches/`     | Model list caches (JSON files) |

## Building from Source

```bash
# Build the Docker image
make docker-build

# Or with custom tag
docker build --build-arg VERSION=$(git describe --tags) -t llmapiproxy:latest .
```

The Dockerfile uses a multi-stage build:

1. **Builder stage** — Compiles the Go binary with version injection
2. **Runtime stage** — Minimal Alpine image with ca-certificates

## Make Targets

| Target              | Description                               |
| ------------------- | ----------------------------------------- |
| `make up`           | Start the container (rebuild if needed)   |
| `make down`         | Stop and remove the container             |
| `make restart`      | Restart the container (rebuild if needed) |
| `make logs`         | Tail container logs                       |
| `make ps`           | Show running containers                   |
| `make shell`        | Open a shell inside the container         |
| `make docker-build` | Build the Docker image                    |

## Reverse Proxy Setup

For production with HTTPS, use a reverse proxy:

### Nginx

```nginx
server {
    listen 443 ssl;
    server_name llmproxy.example.com;

    ssl_certificate /path/to/cert.pem;
    ssl_certificate_key /path/to/key.pem;

    location / {
        proxy_pass http://127.0.0.1:8000;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;

        # SSE streaming support
        proxy_buffering off;
        proxy_cache off;
        proxy_read_timeout 300s;
    }
}
```

### Caddy

```
llmproxy.example.com {
    reverse_proxy localhost:8000
}
```

## Setting the Domain

When running behind a reverse proxy or on a remote server, set the `domain` option so OAuth callbacks and UI links work correctly:

```yaml
server:
  domain: "https://llmproxy.example.com"
```

Or for Tailscale:

```yaml
server:
  domain: "myserver.tail:8000"
```

## Environment Variables

The Docker image uses these defaults:

- **Entrypoint:** `./llmapiproxy serve --config /app/data/config.yaml`
- **Working directory:** `/app`
- **Data volume:** `/data` → mounted to `/app/data`
- **Default port:** `8080` (inside container; map to any host port)

## Health Checks

The built-in health check uses `GET /health`:

```json
{ "status": "ok" }
```

Status is `"degraded"` if any OAuth backend requires re-authentication.

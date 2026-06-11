# --------------------------------------------------
# dtk — Docker Time Keeper
# Multi-stage build: tiny final image (~10 MB)
# --------------------------------------------------

# ---- builder ----
FROM golang:1.24-alpine AS builder

WORKDIR /src

# Cache dependencies first.
COPY go.mod go.sum ./
RUN go mod download

# Build a fully static binary (no CGO needed).
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
      -ldflags="-s -w" \
      -o /dtk .

# ---- final ----
FROM alpine:latest

# ca-certificates lets dtk dial HTTPS upstreams if needed.
RUN apk add --no-cache ca-certificates tzdata

COPY --from=builder /dtk /usr/local/bin/dtk

# Config is mounted at /etc/dtk/config.yaml by default.
VOLUME ["/etc/dtk"]

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/dtk"]
CMD ["-config", "/etc/dtk/config.yaml"]

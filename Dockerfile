# Stage 1: Build
FROM golang:1.26.2-alpine3.23 AS builder

ARG VERSION=dev

WORKDIR /build

# Install build dependencies
RUN apk add --no-cache git

# Download dependencies first (layer cache)
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o llmapiproxy ./cmd/llmapiproxy

# Stage 2: Runtime
FROM alpine:3.23

# ca-certificates needed for HTTPS calls to AI backends
RUN apk add --no-cache ca-certificates curl

WORKDIR /app

COPY --from=builder /build/llmapiproxy .

# /data is the recommended mount point for persistent DB files
VOLUME ["/data"]

EXPOSE 8080

ENTRYPOINT ["./llmapiproxy", "serve", "--config", "/app/data/config.yaml"]

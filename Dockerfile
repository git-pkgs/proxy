FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine AS builder

WORKDIR /src

# Install build dependencies
RUN apk add --no-cache git

# Copy go mod files first for caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the binary for the target platform
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -ldflags="-s -w" -o /proxy ./cmd/proxy

FROM alpine:3.24.2

RUN apk add --no-cache ca-certificates git

COPY --from=builder /proxy /usr/local/bin/proxy

# Create non-root user
RUN adduser -D -u 1000 proxy
USER proxy

# Default data directory
WORKDIR /data

EXPOSE 8080

# /health checks the database and storage and can take up to ten seconds.
HEALTHCHECK --interval=30s --timeout=11s --start-period=10s --retries=3 \
  CMD wget -q -O /dev/null http://127.0.0.1:8080/health || exit 1

ENTRYPOINT ["proxy"]
CMD ["serve"]

FROM golang:1.24-alpine AS builder

WORKDIR /src

# Install build dependencies
RUN apk add --no-cache git

# Copy go mod files first for caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the binary
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /proxy ./cmd/proxy

FROM alpine:3.21

RUN apk add --no-cache ca-certificates

COPY --from=builder /proxy /usr/local/bin/proxy

# Create non-root user
RUN adduser -D -u 1000 proxy
USER proxy

# Default data directory
WORKDIR /data

EXPOSE 8080

ENTRYPOINT ["proxy"]
CMD ["serve"]

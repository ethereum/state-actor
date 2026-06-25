# Build stage
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git make

WORKDIR /app

# Copy go mod files first for better caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Build
ARG STATE_ACTOR_VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w -X main.Version=${STATE_ACTOR_VERSION}" -o state-actor .

# Runtime stage
FROM alpine:3.19

LABEL org.opencontainers.image.title="State Actor"
LABEL org.opencontainers.image.description="High-performance Ethereum state generator"
LABEL org.opencontainers.image.source="https://github.com/ethereum/state-actor"
LABEL org.opencontainers.image.licenses="MIT"
ARG STATE_ACTOR_VERSION=dev
ARG STATE_ACTOR_REVISION=
LABEL org.opencontainers.image.version="${STATE_ACTOR_VERSION}"
LABEL org.opencontainers.image.revision="${STATE_ACTOR_REVISION}"

RUN apk add --no-cache ca-certificates

COPY --from=builder /app/state-actor /usr/local/bin/

# Default output directory
VOLUME /output

ENTRYPOINT ["state-actor"]
CMD ["--help"]

# syntax=docker/dockerfile:1

FROM golang:1.24-alpine AS build
WORKDIR /src

# No third-party dependencies, but keep the layer cache friendly.
COPY go.mod ./
RUN go mod download

COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
        -o /out/audiosocket-bridge ./cmd/audiosocket-bridge \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
        -o /out/audiosocket-tester ./cmd/audiosocket-tester

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata pulseaudio-utils \
    && adduser -D -H -u 10001 -s /sbin/nologin bridge

COPY --from=build /out/audiosocket-bridge /usr/local/bin/audiosocket-bridge
COPY --from=build /out/audiosocket-tester /usr/local/bin/audiosocket-tester

USER bridge

ENV BRIDGE_MODE=echo \
    BRIDGE_LISTEN_ADDR=0.0.0.0:9092 \
    BRIDGE_HEALTH_ADDR=0.0.0.0:8080 \
    BRIDGE_LOG_FORMAT=json

# Only the health endpoint is exposed. The AudioSocket listener on 9092 stays
# private to the Docker network and is never published to the host.
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -q -O - http://127.0.0.1:8080/healthz >/dev/null 2>&1 || exit 1

ENTRYPOINT ["/usr/local/bin/audiosocket-bridge"]

# Pin base images by digest for reproducible/secure production builds.
FROM golang:1.27-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS build

WORKDIR /src
RUN apk add --no-cache ca-certificates
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/interim ./cmd/interim

FROM alpine:3.24@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S interim \
    && adduser -S -G interim -h /data interim \
    && mkdir -p /data \
    && chown interim:interim /data
COPY --from=build /out/interim /usr/local/bin/interim
ENV DATA_DIR=/data LISTEN_ADDR=:8080
WORKDIR /data
USER interim
EXPOSE 8080
VOLUME ["/data"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null || exit 1
ENTRYPOINT ["/usr/local/bin/interim"]

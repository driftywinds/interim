FROM golang:1.27-alpine AS build

WORKDIR /src
RUN apk add --no-cache ca-certificates
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/interim ./cmd/interim

FROM alpine:3.22
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

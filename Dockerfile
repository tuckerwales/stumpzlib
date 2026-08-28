FROM golang:1.22-alpine AS build

WORKDIR /src

# No third-party dependencies, but keep the layer split so a source-only
# change doesn't invalidate the module cache.
COPY go.mod ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /stumpzlib .


FROM alpine:3.20

# Needed to verify TLS when talking to the catalog API and to Stump over HTTPS.
RUN apk add --no-cache ca-certificates wget

COPY --from=build /stumpzlib /usr/local/bin/stumpzlib

ENV LISTEN=:8080
EXPOSE 8080

# Unhealthy when the library volume isn't mounted or isn't writable.
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["stumpzlib"]

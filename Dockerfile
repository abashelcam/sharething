# ---------- build stage ----------
FROM golang:1.23-alpine AS build

WORKDIR /src

# No external dependencies, so go.mod alone is enough.
COPY go.mod ./
COPY main.go ./
COPY static/ ./static/

# The whole static/ directory is embedded into the binary via //go:embed,
# so the runtime image needs nothing but the binary itself.
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags="-s -w" \
        -o /out/fileshare .

# ---------- runtime stage ----------
FROM alpine:3.20

# tzdata: Go needs the zoneinfo database for the TZ env var to work,
#         which decides when share expiry actually fires.
# wget:   busybox wget, used by the healthcheck below.
RUN apk add --no-cache ca-certificates tzdata

COPY --from=build /out/fileshare /usr/local/bin/fileshare

# The app writes config.json, shares.json and uploads/ relative to the
# working directory, so /data is the single thing worth persisting.
WORKDIR /data
VOLUME /data

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/api/health || exit 1

ENTRYPOINT ["/usr/local/bin/fileshare"]

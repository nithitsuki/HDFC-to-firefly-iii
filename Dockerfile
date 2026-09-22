# syntax=docker/dockerfile:1

# ---- build ------------------------------------------------------------------
FROM golang:1.27-alpine AS build

WORKDIR /src

# Copy the module files first so dependency download is cached separately from
# source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

# CGO_ENABLED=0 keeps this a static binary, so the runtime stage needs no libc.
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath -ldflags="-s -w" \
      -o /hdfc2ff ./cmd/hdfc2ff

# ---- runtime ----------------------------------------------------------------
# distroless: no shell, no package manager, non-root by default. The binary is
# static and the timezone database is embedded, so nothing else is needed.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /hdfc2ff /hdfc2ff

# The SQLite state lives here. Mount a volume or the importer forgets every
# message it has seen on restart, and re-imports the whole lookback window.
WORKDIR /data
VOLUME ["/data"]

USER nonroot:nonroot

ENTRYPOINT ["/hdfc2ff"]
CMD ["run"]

# Meaningful, not just "is the process alive": healthcheck reads the timestamp
# of the last poll that completed successfully. A service that is up but failing
# every cycle reports unhealthy.
HEALTHCHECK --interval=5m --timeout=30s --start-period=2m --retries=3 \
  CMD ["/hdfc2ff", "healthcheck", "-max-age", "15m"]

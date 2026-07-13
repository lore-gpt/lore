# syntax=docker/dockerfile:1

# --- build stage: compile a fully static, stripped binary ---
# Base images are digest-pinned; Renovate (renovate.json) refreshes the digests
# so the pin never freezes out upstream security patches. Tag kept for readability.
FROM golang:1.26-bookworm@sha256:18aedc16aa19b3fd7ded7245fc14b109e054d65d22ed53c355c899582bbb2113 AS build
WORKDIR /src

# Module cache layer — only re-downloads when go.mod/go.sum change.
COPY go.mod go.sum ./
RUN go mod download

# Build. CGO off => no libc dependency, so the binary runs on distroless/static.
# -trimpath + -ldflags="-s -w" strip paths and debug info for a smaller image.
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath -ldflags="-s -w" \
    -o /out/lore ./server/cmd/lore

# --- runtime stage: distroless/static, nonroot, no shell ---
# CA certificates and tzdata are bundled. There is no curl, so the container
# HEALTHCHECK (in docker-compose) uses `lore health`, which probes /healthz.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:b7bb25d9f7c31d2bdd1982feb4dafcaf137703c7075dbe2febb41c24212b946f
COPY --from=build /out/lore /lore
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/lore"]

# syntax=docker/dockerfile:1

# --- build stage: compile a fully static, stripped binary ---
FROM golang:1.26-bookworm AS build
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
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/lore /lore
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/lore"]

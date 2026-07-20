# syntax=docker/dockerfile:1

# --- build stage: install deps and build the standalone Next server ---
# Pinned to the NATIVE builder arch (--platform=$BUILDPLATFORM): with image optimization off (next.config
# images.unoptimized), the Next standalone output is arch-independent JavaScript — no native `sharp` binary —
# so a multi-arch build (amd64 + arm64) builds ONCE here and the per-arch runtime stages just COPY the same
# bundle, with no QEMU emulation of the Node build. (Mirrors the server image's GOARCH cross-compile.)
FROM --platform=$BUILDPLATFORM node:22-bookworm-slim AS build
WORKDIR /app
RUN corepack enable && corepack prepare pnpm@11 --activate

# Dependency layer — re-installs only when the manifest or lockfile changes.
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml ./
RUN pnpm install --frozen-lockfile

# Build. The OpenAPI wire types are committed, so no codegen (and no ../spec) is
# needed in the image build context.
COPY . .
# Build, then drop sharp's native binaries. Image optimization is disabled (next.config images.unoptimized),
# so sharp is never loaded — but Next still traces it into the standalone bundle, and shipping the build-arch
# binary would tie the image to one architecture. Removing the @img/sharp directories leaves an
# arch-independent, pure-JS bundle, so the single native build above ships to both amd64 and arm64 (no QEMU).
RUN pnpm build && \
    find .next/standalone -type d \( -name '@img' -o -name 'sharp' \) -prune -exec rm -rf {} +

# --- runtime stage: the slim standalone bundle only ---
FROM node:22-bookworm-slim AS runner
WORKDIR /app
ENV NODE_ENV=production \
    PORT=3000 \
    HOSTNAME=0.0.0.0

# Next's standalone output bundles a minimal node_modules with server.js, so the
# runtime needs no install — just the server plus the static and public assets.
COPY --from=build /app/.next/standalone ./
COPY --from=build /app/.next/static ./.next/static
COPY --from=build /app/public ./public

# Runs as root (no USER) so it can read the root-owned credentials file the compose
# stack mounts read-only. This is a local-dev diagnostic container bound to
# localhost — do not expose it to a network.
EXPOSE 3000
CMD ["node", "server.js"]

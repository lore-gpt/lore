# Lore Inspector

A read-only, self-host **diagnostic UI** for a Lore server. Browse what a project
has stored — memories, their versions, and run traces — and soft-delete a memory
when you need to. It talks to the server's REST API through a small server-side
proxy, so the browser never holds your API key.

It is a diagnostic window, not a control plane: there is no login, no accounts, and
no sign-up. Protect it at the network layer (bind to localhost, or put a
reverse-proxy with auth in front) — do not expose it directly to the internet.

## Develop

```bash
pnpm install
# point at your running Lore server (default: http://localhost:8080)
export LORE_API_URL=http://localhost:8080
pnpm dev
```

Open http://localhost:3000. If no server-side key is configured, you'll be asked
to paste a project API key (create one with `lore keys create`). The key is stored
in an httpOnly, SameSite=Strict session cookie; the browser cannot read it back,
and it is cleared when the browser closes.

To skip the connect screen entirely (e.g. in a container), set a server-side key:

```bash
export LORE_API_KEY=lore_sk_...
```

A server-side key always takes priority over a browser-supplied one.

## Build

```bash
pnpm build   # produces a standalone Next.js server
pnpm start
```

## Tasks

- `pnpm gen` — regenerate wire types from `../spec/openapi.yaml`
- `pnpm gen:check` — fail if the generated types drift from the spec
- `pnpm check` — Biome lint + format check
- `pnpm build` — type-check and build

## Attribution

The UI skeleton, the shadcn/ui primitives under `src/components/ui`, and the theme
system are derived from the MIT-licensed **next-shadcn-admin-dashboard**. See
[`NOTICE`](./NOTICE).

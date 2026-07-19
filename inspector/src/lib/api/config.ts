// The upstream Lore server the Inspector's BFF proxies to.
// Server-to-server only (never exposed to the browser), so no CORS is involved.
export const LORE_API_URL = (process.env.LORE_API_URL ?? "http://localhost:8080").replace(/\/+$/, "");

"use client";

// NOTE: this file is excluded from Biome in biome.json — Biome 2.5.x panics with an
// internal module-resolver error while processing it (non-fatal, but it skips the
// file). Keep it small and simple; `next build` still type-checks it.

import { useState, useTransition } from "react";

import { connect } from "@/server/session-actions";

// Native elements (styled to match the shadcn primitives) keep this leaf form
// free of `@/components/ui/*` imports.
export function ConnectForm({ next }: { next?: string }) {
  const [error, setError] = useState<string | null>(null);
  const [pending, startTransition] = useTransition();

  function handleAction(formData: FormData) {
    setError(null);
    startTransition(async () => {
      // `connect` redirects on success and only returns when there is an error.
      const result = await connect(formData);
      if (result) {
        setError(result.message);
      }
    });
  }

  return (
    <form action={handleAction} className="grid gap-4">
      {next ? <input type="hidden" name="next" value={next} /> : null}
      <div className="grid gap-2">
        <label htmlFor="apiKey" className="font-medium text-sm">
          API key
        </label>
        <input
          id="apiKey"
          name="apiKey"
          type="password"
          autoComplete="off"
          placeholder="lore_sk_..."
          spellCheck={false}
          required
          className="h-9 w-full rounded-md border border-input bg-transparent px-3 py-1 text-sm shadow-xs outline-none focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/50"
        />
        <p className="text-muted-foreground text-xs">
          Create one with <span className="font-mono">lore keys create</span>. It is stored in an httpOnly cookie —
          the browser never reads it back.
        </p>
      </div>
      {error ? <p className="text-destructive text-sm">{error}</p> : null}
      <button
        type="submit"
        disabled={pending}
        className="inline-flex h-9 items-center justify-center rounded-md bg-primary px-4 font-medium text-primary-foreground text-sm shadow-xs transition-colors hover:bg-primary/90 disabled:pointer-events-none disabled:opacity-50"
      >
        {pending ? "Connecting..." : "Connect"}
      </button>
    </form>
  );
}

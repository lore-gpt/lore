"use client";

import { useState } from "react";

import { ChevronRight } from "lucide-react";

import { apiFetch } from "@/lib/api/client";
import { LoreApiError } from "@/lib/api/errors";
import type { Memory } from "@/lib/api/types";
import { cn } from "@/lib/utils";

type LoadState = "idle" | "loading" | "loaded" | "tombstone" | "error";

// A memory id in the diff. Content is fetched lazily on first expand (via the BFF)
// and memoized — reopening never refetches. A 404 means the memory has since been
// deleted or superseded, shown as a tombstone rather than a broken row.
export function DiffRow({ memoryId }: { memoryId: string }) {
  const [open, setOpen] = useState(false);
  const [state, setState] = useState<LoadState>("idle");
  const [content, setContent] = useState<string | null>(null);

  function toggle() {
    const next = !open;
    setOpen(next);
    // Fetch on first expand, and again on re-expand after a transient error;
    // a loaded body or a 404 tombstone is terminal (memoized, never refetched).
    if (next && (state === "idle" || state === "error")) {
      setState("loading");
      apiFetch<Memory>(`v1/memories/${encodeURIComponent(memoryId)}`)
        .then((memory) => {
          setContent(memory.content);
          setState("loaded");
        })
        .catch((err) => {
          setState(err instanceof LoreApiError && err.status === 404 ? "tombstone" : "error");
        });
    }
  }

  return (
    <div className="rounded-md border">
      <button
        type="button"
        onClick={toggle}
        className="flex w-full items-center gap-2 px-2 py-1.5 text-left font-mono text-xs hover:bg-muted"
        aria-expanded={open}
      >
        <ChevronRight className={cn("size-3.5 shrink-0 transition-transform", open && "rotate-90")} />
        <span className="truncate">{memoryId}</span>
      </button>
      {open ? (
        <div className="border-t px-3 py-2">
          {state === "loading" ? <span className="text-muted-foreground text-xs">Loading…</span> : null}
          {state === "loaded" ? <p className="whitespace-pre-wrap break-words text-sm">{content}</p> : null}
          {state === "tombstone" ? (
            <span className="text-muted-foreground text-xs">
              This memory is no longer live (deleted or superseded).
            </span>
          ) : null}
          {state === "error" ? <span className="text-destructive text-xs">Could not load this memory.</span> : null}
        </div>
      ) : null}
    </div>
  );
}

"use client";

import { useRouter, useSearchParams } from "next/navigation";
import { useEffect, useRef, useState, useTransition } from "react";

import { Search } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select";

const KINDS = ["working", "semantic", "episodic", "procedural"] as const;

// The one client island on the memories list (per the locked design): the search
// box is debounced (no navigation per keystroke) and updates via `replace` so it
// never spams browser history; every other filter commits on Apply/Enter via
// `push`. Any commit drops the pagination cursor, so a new filter never pairs a
// stale cursor (which the server would reject as invalid_cursor).
export function MemoriesFilters() {
  const router = useRouter();
  const sp = useSearchParams();
  const [pending, startTransition] = useTransition();

  const [q, setQ] = useState(sp.get("q") ?? "");
  const [kind, setKind] = useState(sp.get("kind") ?? "");
  const [runId, setRunId] = useState(sp.get("run_id") ?? "");
  const [trust, setTrust] = useState(sp.get("trust_tier") ?? "");
  const [review, setReview] = useState(sp.get("review_status") ?? "");

  const debounce = useRef<ReturnType<typeof setTimeout> | null>(null);
  // Set before any navigation this island initiates, so the resync effect can tell
  // our own push/replace apart from an external navigation.
  const selfNav = useRef(false);

  // Resync the controls to the URL ONLY on external navigation (back/forward, a
  // shared link). Skipping our own navigations is what keeps a debounced search
  // push from wiping an un-applied dropdown edit or an in-flight keystroke.
  useEffect(() => {
    if (selfNav.current) {
      selfNav.current = false;
      return;
    }
    // External navigation supersedes any pending debounce (else it would fire with
    // a stale filter snapshot and revert the URL we just navigated to).
    if (debounce.current) {
      clearTimeout(debounce.current);
    }
    setQ(sp.get("q") ?? "");
    setKind(sp.get("kind") ?? "");
    setRunId(sp.get("run_id") ?? "");
    setTrust(sp.get("trust_tier") ?? "");
    setReview(sp.get("review_status") ?? "");
  }, [sp]);

  function navigate(qs: URLSearchParams, mode: "push" | "replace") {
    selfNav.current = true;
    const href = qs.toString() ? `/memories?${qs}` : "/memories";
    startTransition(() => router[mode](href));
  }

  // Apply the current filters (committed via a history entry). Cursor is dropped.
  function apply() {
    if (debounce.current) {
      clearTimeout(debounce.current);
    }
    const qs = new URLSearchParams();
    const entries: [string, string][] = [
      ["q", q],
      ["kind", kind],
      ["run_id", runId],
      ["trust_tier", trust],
      ["review_status", review],
    ];
    for (const [key, value] of entries) {
      if (value.trim()) {
        qs.set(key, value.trim());
      }
    }
    navigate(qs, "push");
  }

  // Debounced live search. It preserves the *committed* filters from the URL (not
  // un-applied local edits to the selects/inputs, which stay pending until Apply)
  // and replaces history so typing never spams the browser back stack.
  function onQChange(next: string) {
    setQ(next);
    if (debounce.current) {
      clearTimeout(debounce.current);
    }
    debounce.current = setTimeout(() => {
      const qs = new URLSearchParams();
      if (next.trim()) {
        qs.set("q", next.trim());
      }
      for (const key of ["kind", "run_id", "trust_tier", "review_status"]) {
        const value = sp.get(key);
        if (value) {
          qs.set(key, value);
        }
      }
      navigate(qs, "replace");
    }, 400);
  }

  function clearAll() {
    if (debounce.current) {
      clearTimeout(debounce.current);
    }
    setQ("");
    setKind("");
    setRunId("");
    setTrust("");
    setReview("");
    navigate(new URLSearchParams(), "push");
  }

  const hasAny = [q, kind, runId, trust, review].some((value) => value.trim());

  return (
    <form
      onSubmit={(event) => {
        event.preventDefault();
        apply();
      }}
      className="flex flex-wrap items-center gap-2"
    >
      <div className="relative min-w-56 flex-1">
        <Search className="-translate-y-1/2 pointer-events-none absolute top-1/2 left-2.5 size-4 text-muted-foreground" />
        <Input
          value={q}
          onChange={(event) => onQChange(event.target.value)}
          placeholder="Search content…"
          aria-label="Search memory content"
          className="pl-8"
        />
      </div>
      <NativeSelect value={kind} onChange={(event) => setKind(event.target.value)} aria-label="Kind" className="w-auto">
        <NativeSelectOption value="">All kinds</NativeSelectOption>
        {KINDS.map((value) => (
          <NativeSelectOption key={value} value={value}>
            {value}
          </NativeSelectOption>
        ))}
      </NativeSelect>
      <Input
        value={runId}
        onChange={(event) => setRunId(event.target.value)}
        placeholder="run_id"
        aria-label="Run id"
        className="w-40 font-mono text-xs"
      />
      <Input
        value={trust}
        onChange={(event) => setTrust(event.target.value)}
        placeholder="trust tier"
        aria-label="Trust tier"
        className="w-32"
      />
      <Input
        value={review}
        onChange={(event) => setReview(event.target.value)}
        placeholder="review status"
        aria-label="Review status"
        className="w-36"
      />
      <Button type="submit" size="sm" disabled={pending}>
        Apply
      </Button>
      {hasAny ? (
        <Button type="button" size="sm" variant="ghost" onClick={clearAll} disabled={pending}>
          Clear
        </Button>
      ) : null}
    </form>
  );
}

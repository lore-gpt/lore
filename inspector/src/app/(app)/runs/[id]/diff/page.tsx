import { headers } from "next/headers";
import Link from "next/link";
import { redirect } from "next/navigation";

import { ArrowLeft, Ghost, Minus, Plus, ServerCrash } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Empty, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty";
import { LoreApiError } from "@/lib/api/errors";
import { findTraceEntries } from "@/lib/api/runs";
import type { RunTraceEntry } from "@/lib/api/types";
import { formatUtc } from "@/lib/format";
import { sanitizeNextPath } from "@/lib/nav";
import { isUuid } from "@/lib/uuid";

import { DiffRow } from "./_components/diff-row";

export const dynamic = "force-dynamic";

type SearchParams = { [key: string]: string | string[] | undefined };

function first(value: string | string[] | undefined): string | undefined {
  return Array.isArray(value) ? value[0] : value;
}

export default async function RunDiffPage({
  params,
  searchParams,
}: {
  params: Promise<{ id: string }>;
  searchParams: Promise<SearchParams>;
}) {
  const { id } = await params;
  const sp = await searchParams;
  const a = first(sp.a);
  const b = first(sp.b);

  if (!a || !b || !isUuid(a) || !isUuid(b) || a === b) {
    return (
      <Shell runId={id}>
        <TraceEmpty
          icon={<ServerCrash />}
          title="Invalid comparison"
          description="A diff needs two distinct pack ids (?a= and ?b=). Pick two packs from the trace."
        />
      </Shell>
    );
  }

  let entries: Map<string, RunTraceEntry> | null = null;
  let errorState: "offline" | "notfound" | "error" | null = null;
  try {
    entries = await findTraceEntries(id, [a, b]);
  } catch (err) {
    if (err instanceof LoreApiError) {
      if (err.status === 401) {
        const here = sanitizeNextPath((await headers()).get("x-pathname")) ?? `/runs/${id}/diff`;
        redirect(`/connect?expired=1&next=${encodeURIComponent(here)}`);
      }
      errorState = err.status === 404 ? "notfound" : "error";
    } else {
      errorState = "offline";
    }
  }

  if (errorState || !entries) {
    const copy = {
      notfound: { title: "Run not found", description: "No run with this id exists in this project." },
      offline: { title: "Server unreachable", description: "Could not reach the Lore server." },
      error: { title: "Could not load the trace", description: "The server returned an unexpected error." },
    }[errorState ?? "error"];
    return (
      <Shell runId={id}>
        <TraceEmpty icon={<ServerCrash />} title={copy.title} description={copy.description} />
      </Shell>
    );
  }

  const entryA = entries.get(a);
  const entryB = entries.get(b);
  if (!entryA || !entryB) {
    return (
      <Shell runId={id}>
        <TraceEmpty
          icon={<Ghost />}
          title="Pack log not found"
          description="One or both packs are no longer in the trace (retention, or beyond the searched range)."
        />
      </Shell>
    );
  }

  const aSet = new Set(entryA.memory_ids);
  const bSet = new Set(entryB.memory_ids);
  const added = entryB.memory_ids.filter((memoryId) => !aSet.has(memoryId));
  const removed = entryA.memory_ids.filter((memoryId) => !bSet.has(memoryId));
  const keptInA = entryA.memory_ids.filter((memoryId) => bSet.has(memoryId));
  const keptInB = entryB.memory_ids.filter((memoryId) => aSet.has(memoryId));
  const reordered = keptInA.join(",") !== keptInB.join(",");

  let hashState: "identical" | "differ" | "na";
  if (!entryA.pack_hash || !entryB.pack_hash) {
    hashState = "na";
  } else if (entryA.pack_hash === entryB.pack_hash) {
    hashState = "identical";
  } else {
    hashState = "differ";
  }

  return (
    <Shell runId={id}>
      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
        <PackCard label="A (baseline)" entry={entryA} />
        <PackCard label="B (compared)" entry={entryB} />
      </div>

      <div className="flex flex-wrap items-center gap-2 text-sm">
        <span className="text-muted-foreground">Pack hash:</span>
        <HashBadge state={hashState} />
        <span className="ml-2 text-muted-foreground">
          {added.length} added · {removed.length} removed · {keptInB.length} kept
        </span>
      </div>

      <DiffSection
        title="Added"
        hint="In B, not in A"
        icon={<Plus className="size-3.5 text-emerald-600 dark:text-emerald-400" />}
        ids={added}
      />
      <DiffSection
        title="Removed"
        hint="In A, not in B"
        icon={<Minus className="size-3.5 text-destructive" />}
        ids={removed}
      />
      <DiffSection
        title="Kept"
        hint="In both"
        badge={reordered ? <Badge variant="secondary">reordered</Badge> : null}
        ids={keptInB}
      />
    </Shell>
  );
}

function Shell({ runId, children }: { runId: string; children: React.ReactNode }) {
  return (
    <div className="flex flex-col gap-5">
      <div>
        <Link
          href={`/runs/${runId}`}
          className="mb-1 inline-flex items-center gap-1.5 text-muted-foreground text-sm hover:text-foreground"
        >
          <ArrowLeft className="size-4" />
          Run trace
        </Link>
        <h1 className="font-heading font-semibold text-2xl tracking-tight">Pack diff</h1>
        <p className="truncate font-mono text-muted-foreground text-xs">{runId}</p>
      </div>
      {children}
    </div>
  );
}

function PackCard({ label, entry }: { label: string; entry: RunTraceEntry }) {
  return (
    <div className="rounded-lg border p-3">
      <div className="flex items-center justify-between gap-2">
        <span className="font-medium text-xs">{label}</span>
        <span className="font-mono text-muted-foreground text-xs">{formatUtc(entry.created_at)}</span>
      </div>
      <p className="mt-1 truncate text-sm" title={entry.query}>
        {entry.query}
      </p>
      <p className="mt-1 text-muted-foreground text-xs">{entry.memory_ids.length} memories</p>
    </div>
  );
}

function HashBadge({ state }: { state: "identical" | "differ" | "na" }) {
  if (state === "na") {
    return <Badge variant="outline">not computed</Badge>;
  }
  if (state === "identical") {
    return <Badge variant="secondary">identical</Badge>;
  }
  return <Badge variant="destructive">differ</Badge>;
}

function DiffSection({
  title,
  hint,
  icon,
  badge,
  ids,
}: {
  title: string;
  hint: string;
  icon?: React.ReactNode;
  badge?: React.ReactNode;
  ids: string[];
}) {
  return (
    <div className="flex flex-col gap-2">
      <div className="flex items-center gap-2">
        {icon}
        <h2 className="font-heading font-medium text-base">{title}</h2>
        <Badge variant="outline">{ids.length}</Badge>
        {badge}
        <span className="text-muted-foreground text-xs">{hint}</span>
      </div>
      {ids.length > 0 ? (
        <div className="flex flex-col gap-1.5">
          {ids.map((memoryId) => (
            <DiffRow key={memoryId} memoryId={memoryId} />
          ))}
        </div>
      ) : (
        <p className="text-muted-foreground text-sm">None.</p>
      )}
    </div>
  );
}

function TraceEmpty({ icon, title, description }: { icon: React.ReactNode; title: string; description: string }) {
  return (
    <Empty className="min-h-64">
      <EmptyHeader>
        <EmptyMedia variant="icon">{icon}</EmptyMedia>
        <EmptyTitle>{title}</EmptyTitle>
        <EmptyDescription>{description}</EmptyDescription>
      </EmptyHeader>
    </Empty>
  );
}

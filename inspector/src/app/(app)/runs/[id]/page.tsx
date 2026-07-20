import { headers } from "next/headers";
import Link from "next/link";
import { redirect } from "next/navigation";

import { ArrowLeft, Ghost, ServerCrash } from "lucide-react";

import { Card } from "@/components/ui/card";
import { Empty, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { LoreApiError } from "@/lib/api/errors";
import { fetchRunTrace } from "@/lib/api/runs";
import type { RunTraceEntry, RunTraceResponse } from "@/lib/api/types";
import { formatUtc } from "@/lib/format";
import { sanitizeNextPath } from "@/lib/nav";

import { PaginationNav } from "../../_components/pagination-nav";
import { RefreshButton } from "../../_components/refresh-button";
import { TrackRecentRun } from "../_components/track-recent-run";
import { DiffForm } from "./_components/diff-form";

export const dynamic = "force-dynamic";

type SearchParams = { [key: string]: string | string[] | undefined };

function first(value: string | string[] | undefined): string | undefined {
  return Array.isArray(value) ? value[0] : value;
}

function fmtMs(value: number | null | undefined): string {
  return typeof value === "number" ? `${value} ms` : "—";
}

function shortHash(value: string | null | undefined): string {
  return value ? value.slice(0, 10) : "—";
}

export default async function RunTracePage({
  params,
  searchParams,
}: {
  params: Promise<{ id: string }>;
  searchParams: Promise<SearchParams>;
}) {
  const { id } = await params;
  const sp = await searchParams;
  const cursor = first(sp.cursor);
  const limit = first(sp.limit);

  let data: RunTraceResponse | null = null;
  let errorState: "offline" | "notfound" | "stale-cursor" | "error" | null = null;

  try {
    data = await fetchRunTrace(id, { cursor, limit });
  } catch (err) {
    if (err instanceof LoreApiError) {
      if (err.status === 401) {
        const here = sanitizeNextPath((await headers()).get("x-pathname")) ?? `/runs/${id}`;
        redirect(`/connect?expired=1&next=${encodeURIComponent(here)}`);
      }
      if (err.status === 404) {
        errorState = "notfound";
      } else if (err.status === 400 && err.code === "invalid_cursor") {
        errorState = "stale-cursor";
      } else {
        errorState = "error";
      }
    } else {
      errorState = "offline";
    }
  }

  function traceHref(nextCursor: string | null): string {
    const qs = new URLSearchParams();
    if (limit?.trim()) {
      qs.set("limit", limit.trim());
    }
    if (nextCursor) {
      qs.set("cursor", nextCursor);
    }
    return qs.toString() ? `/runs/${id}?${qs}` : `/runs/${id}`;
  }

  const packs = data?.packs ?? [];

  let body: React.ReactNode;
  if (errorState) {
    body = <TraceError state={errorState} runId={id} />;
  } else if (packs.length === 0) {
    body = (
      <Empty className="min-h-64">
        <EmptyHeader>
          <EmptyMedia variant="icon">
            <Ghost />
          </EmptyMedia>
          <EmptyTitle>No packs yet</EmptyTitle>
          <EmptyDescription>This run has no context-pack history — nothing has packed from it.</EmptyDescription>
        </EmptyHeader>
      </Empty>
    );
  } else {
    body = (
      <div className="flex flex-col gap-3">
        <TrackRecentRun runId={id} />
        <p className="text-muted-foreground text-sm">
          {packs.length} pack{packs.length === 1 ? "" : "s"} on this page (newest first).
        </p>
        <DiffForm runId={id}>
          <Card className="overflow-hidden p-0">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead className="w-24">Compare</TableHead>
                  <TableHead className="w-40">Time</TableHead>
                  <TableHead>Query</TableHead>
                  <TableHead className="w-16 text-right">Mem</TableHead>
                  <TableHead className="w-24 text-right">Latency</TableHead>
                  <TableHead className="w-28 text-right">Freshness</TableHead>
                  <TableHead className="w-28">Hash</TableHead>
                  <TableHead className="w-24" />
                </TableRow>
              </TableHeader>
              <TableBody>
                {packs.map((entry, index) => (
                  <PackRow
                    key={entry.id}
                    entry={entry}
                    runId={id}
                    olderId={index < packs.length - 1 ? packs[index + 1].id : null}
                  />
                ))}
              </TableBody>
            </Table>
          </Card>
        </DiffForm>
        <PaginationNav
          hasCursor={Boolean(cursor)}
          hasMore={data?.has_more ?? false}
          firstPageHref={traceHref(null)}
          nextHref={data?.next_cursor ? traceHref(data.next_cursor) : null}
        />
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-6">
      <div className="flex items-start justify-between gap-4">
        <div className="min-w-0">
          <Link
            href="/runs"
            className="mb-1 inline-flex items-center gap-1.5 text-muted-foreground text-sm hover:text-foreground"
          >
            <ArrowLeft className="size-4" />
            Runs
          </Link>
          <h1 className="font-heading font-semibold text-2xl tracking-tight">Run trace</h1>
          <p className="truncate font-mono text-muted-foreground text-xs">{id}</p>
        </div>
        <RefreshButton />
      </div>
      {body}
    </div>
  );
}

function PackRow({ entry, runId, olderId }: { entry: RunTraceEntry; runId: string; olderId: string | null }) {
  return (
    <TableRow>
      <TableCell>
        <div className="flex items-center gap-2 text-xs">
          <label className="inline-flex items-center gap-1">
            <input type="radio" name="ab_a" value={entry.id} className="accent-primary" aria-label="Select as A" />A
          </label>
          <label className="inline-flex items-center gap-1">
            <input type="radio" name="ab_b" value={entry.id} className="accent-primary" aria-label="Select as B" />B
          </label>
        </div>
      </TableCell>
      <TableCell className="font-mono text-muted-foreground text-xs">{formatUtc(entry.created_at)}</TableCell>
      <TableCell className="max-w-0">
        <span className="block truncate">{entry.query}</span>
      </TableCell>
      <TableCell className="text-right font-mono text-sm tabular-nums">{entry.memory_ids.length}</TableCell>
      <TableCell className="text-right font-mono text-muted-foreground text-xs">{fmtMs(entry.latency_ms)}</TableCell>
      <TableCell className="text-right font-mono text-muted-foreground text-xs">
        {fmtMs(entry.freshness_lag_ms)}
      </TableCell>
      <TableCell className="font-mono text-muted-foreground text-xs">{shortHash(entry.pack_hash)}</TableCell>
      <TableCell>
        {olderId ? (
          <Link
            href={`/runs/${runId}/diff?a=${olderId}&b=${entry.id}`}
            className="text-primary text-xs hover:underline"
          >
            diff vs prev
          </Link>
        ) : null}
      </TableCell>
    </TableRow>
  );
}

function TraceError({ state, runId }: { state: "offline" | "notfound" | "stale-cursor" | "error"; runId: string }) {
  const copy = {
    notfound: {
      icon: <Ghost />,
      title: "Run not found",
      description: "No run with this id exists in this project.",
    },
    offline: {
      icon: <ServerCrash />,
      title: "Server unreachable",
      description: "Could not reach the Lore server.",
    },
    "stale-cursor": {
      icon: <ServerCrash />,
      title: "This page link has expired",
      description: "The pagination cursor is stale.",
    },
    error: {
      icon: <ServerCrash />,
      title: "Could not load the trace",
      description: "The server returned an unexpected error.",
    },
  }[state];

  return (
    <Empty className="min-h-64">
      <EmptyHeader>
        <EmptyMedia variant="icon">{copy.icon}</EmptyMedia>
        <EmptyTitle>{copy.title}</EmptyTitle>
        <EmptyDescription>
          {copy.description}{" "}
          {state === "stale-cursor" ? (
            <Link href={`/runs/${runId}`} className="underline">
              Back to the first page
            </Link>
          ) : null}
        </EmptyDescription>
      </EmptyHeader>
    </Empty>
  );
}

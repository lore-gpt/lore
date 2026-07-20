import { headers } from "next/headers";
import Link from "next/link";
import { redirect } from "next/navigation";

import { Boxes, ServerCrash, Timer } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Card } from "@/components/ui/card";
import { Empty, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { LoreApiError } from "@/lib/api/errors";
import { fetchMemories, type MemoryListParams } from "@/lib/api/memories";
import type { Memory, MemoryListResponse } from "@/lib/api/types";
import { formatUtc } from "@/lib/format";
import { sanitizeNextPath } from "@/lib/nav";

import { RefreshButton } from "../_components/refresh-button";
import { DeleteToast } from "./_components/delete-toast";
import { ListUrlMemory } from "./_components/list-url-memory";
import { MemoriesFilters } from "./_components/memories-filters";
import { PaginationNav } from "./_components/pagination-nav";

export const dynamic = "force-dynamic";

type SearchParams = { [key: string]: string | string[] | undefined };

function first(value: string | string[] | undefined): string | undefined {
  return Array.isArray(value) ? value[0] : value;
}

export default async function MemoriesPage({ searchParams }: { searchParams: Promise<SearchParams> }) {
  const sp = await searchParams;
  const params: MemoryListParams = {
    q: first(sp.q),
    kind: first(sp.kind),
    run_id: first(sp.run_id),
    trust_tier: first(sp.trust_tier),
    review_status: first(sp.review_status),
    cursor: first(sp.cursor),
    limit: first(sp.limit),
  };
  // Trim once so the search decision, the API call, the echoed label, and the
  // active-filter summary all agree on the effective query.
  const query = params.q?.trim() ?? "";
  const isSearch = query.length > 0;

  let data: MemoryListResponse | null = null;
  let errorState: "offline" | "stale-cursor" | "error" | null = null;

  try {
    data = await fetchMemories(params);
  } catch (err) {
    if (err instanceof LoreApiError) {
      if (err.status === 401) {
        const here = sanitizeNextPath((await headers()).get("x-pathname")) ?? "/memories";
        redirect(`/connect?expired=1&next=${encodeURIComponent(here)}`);
      }
      errorState = err.status === 400 && err.code === "invalid_cursor" ? "stale-cursor" : "error";
    } else {
      errorState = "offline";
    }
  }

  const activeFilters = [
    query && `q: ${query}`,
    params.kind?.trim() && `kind: ${params.kind.trim()}`,
    params.run_id?.trim() && `run: ${params.run_id.trim()}`,
    params.trust_tier?.trim() && `trust: ${params.trust_tier.trim()}`,
    params.review_status?.trim() && `review: ${params.review_status.trim()}`,
  ].filter(Boolean) as string[];

  // Build a browse-mode URL carrying the active filters (never q — pagination is
  // browse-only). With no cursor it is the filtered first page; with a cursor it
  // is the next page. Both preserve the filters so paging and "First page" never
  // silently drop what the operator is viewing.
  function browseHref(cursor: string | null): string {
    const qs = new URLSearchParams();
    for (const key of ["kind", "run_id", "trust_tier", "review_status", "limit"] as const) {
      const value = params[key]?.trim();
      if (value) {
        qs.set(key, value);
      }
    }
    if (cursor) {
      qs.set("cursor", cursor);
    }
    return qs.toString() ? `/memories?${qs}` : "/memories";
  }

  const memories = data?.memories ?? [];

  let body: React.ReactNode;
  if (errorState) {
    body = <ErrorState state={errorState} />;
  } else if (memories.length === 0) {
    body = (
      <Empty className="min-h-64">
        <EmptyHeader>
          <EmptyMedia variant="icon">
            <Boxes />
          </EmptyMedia>
          <EmptyTitle>No memories match</EmptyTitle>
          <EmptyDescription>
            {activeFilters.length > 0
              ? `Active filters — ${activeFilters.join(" · ")}.`
              : "This project has no memories yet."}
          </EmptyDescription>
        </EmptyHeader>
      </Empty>
    );
  } else {
    body = (
      <div className="flex flex-col gap-3">
        <p className="text-muted-foreground text-sm">
          {isSearch
            ? `First ${memories.length} result${memories.length === 1 ? "" : "s"} for “${query}” (lexical search).`
            : `${memories.length} memor${memories.length === 1 ? "y" : "ies"} on this page.`}
        </p>
        <Card className="overflow-hidden p-0">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="w-28">Kind</TableHead>
                <TableHead>Content</TableHead>
                <TableHead className="w-28">Trust</TableHead>
                <TableHead className="w-32">Review</TableHead>
                <TableHead className="w-14 text-right">Ver</TableHead>
                <TableHead className="w-40">Created</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {memories.map((memory) => (
                <MemoryRow key={memory.id} memory={memory} />
              ))}
            </TableBody>
          </Table>
        </Card>

        {/* Keyset pagination is browse-only; search mode returns the first N (no cursor). */}
        {isSearch ? null : (
          <PaginationNav
            hasCursor={Boolean(params.cursor)}
            hasMore={data?.has_more ?? false}
            firstPageHref={browseHref(null)}
            nextHref={data?.next_cursor ? browseHref(data.next_cursor) : null}
          />
        )}
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-6">
      <DeleteToast />
      <ListUrlMemory />
      <div className="flex items-start justify-between gap-4">
        <div>
          <h1 className="font-heading font-semibold text-2xl tracking-tight">Memories</h1>
          <p className="text-muted-foreground text-sm">
            Currently-valid memories stored on this server. Soft-deleted and superseded rows are excluded.
          </p>
        </div>
        <RefreshButton />
      </div>

      <MemoriesFilters />

      {body}
    </div>
  );
}

function MemoryRow({ memory }: { memory: Memory }) {
  return (
    <TableRow>
      <TableCell>
        <Badge variant="outline" className="font-normal">
          {memory.kind}
        </Badge>
      </TableCell>
      <TableCell className="max-w-0">
        <Link href={`/memories/${memory.id}`} className="block truncate font-medium hover:underline">
          {memory.content}
        </Link>
      </TableCell>
      <TableCell className="text-muted-foreground text-sm">{memory.trust_tier}</TableCell>
      <TableCell className="text-muted-foreground text-sm">{memory.review_status}</TableCell>
      <TableCell className="text-right font-mono text-sm tabular-nums">{memory.version}</TableCell>
      <TableCell className="font-mono text-muted-foreground text-xs">{formatUtc(memory.created_at)}</TableCell>
    </TableRow>
  );
}

function ErrorState({ state }: { state: "offline" | "stale-cursor" | "error" }) {
  const copy = {
    offline: {
      icon: <ServerCrash />,
      title: "Server unreachable",
      description: "Could not reach the Lore server. Check that it is running and reachable from the Inspector.",
    },
    "stale-cursor": {
      icon: <Timer />,
      title: "This page link has expired",
      description: "The pagination cursor is stale. Reset to the first page and browse again.",
    },
    error: {
      icon: <ServerCrash />,
      title: "Could not load memories",
      description: "The server returned an unexpected error. Try refreshing.",
    },
  }[state];

  return (
    <Empty className="min-h-64">
      <EmptyHeader>
        <EmptyMedia variant="icon">{copy.icon}</EmptyMedia>
        <EmptyTitle>{copy.title}</EmptyTitle>
        <EmptyDescription>
          {copy.description}
          {state === "stale-cursor" ? (
            <>
              {" "}
              <Link href="/memories" className="underline">
                Back to the first page
              </Link>
              .
            </>
          ) : null}
        </EmptyDescription>
      </EmptyHeader>
    </Empty>
  );
}

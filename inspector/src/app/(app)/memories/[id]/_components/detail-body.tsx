import { Ghost, ServerCrash } from "lucide-react";

import { Empty, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty";

import type { MemoryDetail } from "../detail-data";
import { MemoryView } from "./memory-view";

// Renders a resolved detail: the rich view for a live memory or a tombstone, or an
// empty state for an unknown id / load error. Shared by the full page and the
// slide-over modal so both are identical.
export function DetailBody({ detail }: { detail: MemoryDetail }) {
  if (detail.kind === "live") {
    return <MemoryView memory={detail.memory} versions={detail.versions} versionsLoaded={detail.versionsLoaded} />;
  }
  if (detail.kind === "tombstone") {
    return <MemoryView memory={null} versions={detail.versions} versionsLoaded={detail.versionsLoaded} />;
  }
  if (detail.kind === "notfound") {
    return (
      <Empty className="min-h-64">
        <EmptyHeader>
          <EmptyMedia variant="icon">
            <Ghost />
          </EmptyMedia>
          <EmptyTitle>Memory not found</EmptyTitle>
          <EmptyDescription>No memory with this id exists in this project.</EmptyDescription>
        </EmptyHeader>
      </Empty>
    );
  }
  return (
    <Empty className="min-h-64">
      <EmptyHeader>
        <EmptyMedia variant="icon">
          <ServerCrash />
        </EmptyMedia>
        <EmptyTitle>{detail.offline ? "Server unreachable" : "Could not load this memory"}</EmptyTitle>
        <EmptyDescription>
          {detail.offline ? "Could not reach the Lore server." : "The server returned an unexpected error."} Try
          refreshing.
        </EmptyDescription>
      </EmptyHeader>
    </Empty>
  );
}

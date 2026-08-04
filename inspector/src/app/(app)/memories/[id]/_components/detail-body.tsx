import { Ghost, ServerCrash } from "lucide-react";

import { Empty, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty";

import type { MemoryDetail } from "../detail-data";
import { MemoryView } from "./memory-view";

// Renders a resolved detail: the rich view for a live memory or a tombstone, or an
// empty state for an unknown id / load error. Shared by the full page and the
// slide-over modal so both are identical.
//
// `addressable` is the one place the two callers differ. On the full page the active
// tab belongs in the URL, so a link to a specific tab can be shared and survives a
// refresh. In the intercepting modal it must not be: the modal renders OVER the
// memories list, whose filter island resyncs its controls from the URL on any
// navigation it did not initiate — so writing ?tab= from the modal would silently
// discard a filter edit the user had typed but not yet applied. The modal is a
// transient overlay and a shared link opens the full page anyway (interception only
// happens on client-side navigation), so it keeps the tab in local state.
export function DetailBody({ detail, addressable = false }: { detail: MemoryDetail; addressable?: boolean }) {
  if (detail.kind === "live") {
    return (
      <MemoryView
        memory={detail.memory}
        versions={detail.versions}
        versionsLoaded={detail.versionsLoaded}
        addressable={addressable}
      />
    );
  }
  if (detail.kind === "tombstone") {
    return (
      <MemoryView
        memory={null}
        versions={detail.versions}
        versionsLoaded={detail.versionsLoaded}
        addressable={addressable}
      />
    );
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

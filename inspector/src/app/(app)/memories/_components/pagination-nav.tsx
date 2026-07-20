import Link from "next/link";

import { ChevronRight, ChevronsLeft } from "lucide-react";

import { Button } from "@/components/ui/button";

// Keyset pagination (browse mode). Next is a real link — middle-click, open in a
// new tab, and copy all work, and every page is a shareable URL. Because keyset
// paging has no cheap "previous" (and a client-side cursor stack was deliberately
// rejected), the back control is an honest "First page" link that carries the
// active filters — it never promises a previous page it cannot reach, and never
// silently drops the filters the operator is browsing. The browser's own Back
// button still steps through visited pages naturally.
export function PaginationNav({
  hasCursor,
  hasMore,
  firstPageHref,
  nextHref,
}: {
  hasCursor: boolean;
  hasMore: boolean;
  firstPageHref: string;
  nextHref: string | null;
}) {
  return (
    <div className="flex items-center justify-between">
      {hasCursor ? (
        <div className="flex items-center gap-2">
          <Button variant="outline" size="sm" asChild>
            <Link
              href={firstPageHref}
              title="Jumps to the first page. Use your browser's Back button for the previous page."
            >
              <ChevronsLeft className="size-4" />
              First page
            </Link>
          </Button>
          {/* Honest label: this never claims to be "Previous". A faint hint keeps
              the browser-Back affordance discoverable. */}
          <span className="hidden text-muted-foreground text-xs sm:inline">
            Browser Back goes to the previous page.
          </span>
        </div>
      ) : (
        <span />
      )}

      {hasMore && nextHref ? (
        <Button variant="outline" size="sm" asChild>
          <Link href={nextHref}>
            Next
            <ChevronRight className="size-4" />
          </Link>
        </Button>
      ) : (
        <Button variant="outline" size="sm" disabled>
          Next
          <ChevronRight className="size-4" />
        </Button>
      )}
    </div>
  );
}

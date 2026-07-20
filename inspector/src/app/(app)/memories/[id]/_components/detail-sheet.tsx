"use client";

import { useState } from "react";

import { useRouter } from "next/navigation";

import { Maximize2 } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";

// The right-side slide-over for the intercepted /memories/[id] route. Open on
// mount; closing (X, overlay, Esc) navigates back, which unwinds the interception
// and reveals the list underneath. The "Full page" control is a NATIVE anchor: a
// hard navigation is not intercepted, so it lands on the standalone full page (the
// same URL). A direct load / refresh of the URL renders the full page too.
export function DetailSheet({ href, children }: { href: string; children: React.ReactNode }) {
  const router = useRouter();
  const [open, setOpen] = useState(true);

  return (
    <Sheet
      open={open}
      onOpenChange={(next) => {
        setOpen(next);
        if (!next) {
          router.back();
        }
      }}
    >
      <SheetContent side="right" className="gap-0 overflow-y-auto p-0 data-[side=right]:sm:max-w-[45rem]">
        <SheetHeader className="flex-row items-center justify-between gap-2 border-b py-4 pr-14 pl-5">
          <SheetTitle className="font-heading">Memory</SheetTitle>
          <SheetDescription className="sr-only">Memory detail and version history.</SheetDescription>
          <Button asChild variant="ghost" size="sm" className="text-muted-foreground">
            <a href={href}>
              <Maximize2 className="size-4" />
              Full page
            </a>
          </Button>
        </SheetHeader>
        <div className="p-5">{children}</div>
      </SheetContent>
    </Sheet>
  );
}

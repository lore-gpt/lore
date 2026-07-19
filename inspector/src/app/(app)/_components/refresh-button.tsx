"use client";

import { useRouter } from "next/navigation";
import { useTransition } from "react";

import { RotateCw } from "lucide-react";

import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

// Re-fetches the server component's live data via a soft refresh (no full page
// reload). The icon spins while the refresh is in flight.
export function RefreshButton() {
  const router = useRouter();
  const [isPending, startTransition] = useTransition();

  return (
    <Button
      variant="outline"
      size="sm"
      aria-label="Refresh"
      disabled={isPending}
      onClick={() => startTransition(() => router.refresh())}
    >
      <RotateCw className={cn("size-4", isPending && "animate-spin")} />
      Refresh
    </Button>
  );
}

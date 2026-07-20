"use client";

import { useState } from "react";

import { useRouter } from "next/navigation";

import { GitCompareArrows } from "lucide-react";

import { Button } from "@/components/ui/button";

// The one client island over the (server-rendered) trace table. The A/B markers are
// native radios inside the server rows; this captures the selection via the form's
// change event and only writes a URL on Compare — the diff is a shareable
// /runs/[id]/diff?a=&b= link. Compare is disabled until two DISTINCT packs are picked.
export function DiffForm({ runId, children }: { runId: string; children: React.ReactNode }) {
  const router = useRouter();
  const [a, setA] = useState<string | null>(null);
  const [b, setB] = useState<string | null>(null);

  function onChange(event: React.ChangeEvent<HTMLFormElement>) {
    const target = event.target;
    if (!(target instanceof HTMLInputElement)) {
      return;
    }
    if (target.name === "ab_a") {
      setA(target.value);
    } else if (target.name === "ab_b") {
      setB(target.value);
    }
  }

  const canCompare = Boolean(a && b && a !== b);

  return (
    <div className="flex flex-col gap-3">
      <div className="flex flex-wrap items-center justify-between gap-3 rounded-lg border bg-muted/30 px-3 py-2">
        <span className="text-muted-foreground text-sm">
          {canCompare ? "Two packs selected — compare their memory sets." : "Mark two packs (A and B) to compare."}
        </span>
        <Button
          size="sm"
          disabled={!canCompare}
          onClick={() => {
            if (a && b && a !== b) {
              router.push(`/runs/${runId}/diff?a=${a}&b=${b}`);
            }
          }}
        >
          <GitCompareArrows className="size-4" />
          Compare
        </Button>
      </div>
      <form onChange={onChange}>{children}</form>
    </div>
  );
}

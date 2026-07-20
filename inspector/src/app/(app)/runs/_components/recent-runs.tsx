"use client";

import { useEffect, useState } from "react";

import Link from "next/link";

import { Route } from "lucide-react";

import { Button } from "@/components/ui/button";

import { clearRecentRuns, getRecentRuns } from "./runs-recents";

export function RecentRuns() {
  const [runs, setRuns] = useState<string[]>([]);

  useEffect(() => {
    setRuns(getRecentRuns());
  }, []);

  if (runs.length === 0) {
    return null;
  }

  return (
    <div className="flex flex-col gap-2">
      <div className="flex items-center justify-between">
        <h2 className="font-heading font-medium text-muted-foreground text-sm">Recent runs</h2>
        <Button
          variant="ghost"
          size="sm"
          onClick={() => {
            clearRecentRuns();
            setRuns([]);
          }}
        >
          Clear
        </Button>
      </div>
      <div className="flex flex-col gap-1">
        {runs.map((id) => (
          <Link
            key={id}
            href={`/runs/${id}`}
            className="inline-flex items-center gap-2 truncate rounded-md border px-3 py-2 font-mono text-sm hover:bg-muted"
          >
            <Route className="size-3.5 shrink-0 text-muted-foreground" />
            {id}
          </Link>
        ))}
      </div>
    </div>
  );
}

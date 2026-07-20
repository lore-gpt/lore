"use client";

import { useState } from "react";

import { useRouter } from "next/navigation";

import { ArrowRight } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { isUuid } from "@/lib/uuid";

// The primary way into a run's trace when there is no list-runs endpoint: paste a
// run id. Validated locally as a UUID before navigating, so a malformed id is a
// local error, not a wasted request. Navigation only — the shareable thing is the
// resulting /runs/[id] URL.
export function RunIdForm() {
  const router = useRouter();
  const [value, setValue] = useState("");
  const [error, setError] = useState<string | null>(null);

  function onSubmit(event: React.FormEvent) {
    event.preventDefault();
    const id = value.trim();
    if (!isUuid(id)) {
      setError("Enter a valid run id (a UUID).");
      return;
    }
    setError(null);
    router.push(`/runs/${id}`);
  }

  return (
    <form onSubmit={onSubmit} className="flex flex-col gap-2">
      <div className="flex items-center gap-2">
        <Input
          value={value}
          onChange={(event) => {
            setValue(event.target.value);
            if (error) {
              setError(null);
            }
          }}
          placeholder="Run id (UUID)…"
          aria-label="Run id"
          className="font-mono"
        />
        <Button type="submit">
          View trace
          <ArrowRight className="size-4" />
        </Button>
      </div>
      {error ? <p className="text-destructive text-sm">{error}</p> : null}
    </form>
  );
}

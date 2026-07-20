"use client";

import { useState } from "react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader } from "@/components/ui/card";
import type { MemoryVersion } from "@/lib/api/types";
import { formatUtc } from "@/lib/format";

import { CopyButton } from "../../_components/copy-button";

const INITIAL = 5;

// Prior versions, newest-first (the caller reverses the API's oldest-first list).
// Each card is version K's content plus its supersession context — reason and the
// actor are shown independently (a version can carry either alone). Content is
// escaped plain text (never markdown) so a stored payload can't forge UI. Long
// chains collapse to the newest five.
export function VersionsList({ versions }: { versions: MemoryVersion[] }) {
  const [showAll, setShowAll] = useState(false);
  const shown = showAll ? versions : versions.slice(0, INITIAL);

  return (
    <div className="flex flex-col gap-3">
      {shown.map((version) => (
        <VersionCard key={version.version} version={version} />
      ))}

      {versions.length > INITIAL && !showAll ? (
        <Button variant="outline" size="sm" className="self-start" onClick={() => setShowAll(true)}>
          Show all {versions.length} versions
        </Button>
      ) : null}
    </div>
  );
}

function VersionCard({ version }: { version: MemoryVersion }) {
  const hasContext = [version.reason, version.changed_by].some(Boolean);

  return (
    <Card>
      <CardHeader className="flex-row items-center justify-between gap-2 space-y-0">
        <div className="flex items-center gap-2">
          <Badge variant="outline">Version {version.version}</Badge>
          <span className="font-mono text-muted-foreground text-xs">{formatUtc(version.created_at)}</span>
        </div>
        <CopyButton value={version.content} />
      </CardHeader>
      <CardContent className="flex flex-col gap-2">
        <p className="whitespace-pre-wrap break-words text-sm">{version.content}</p>
        {hasContext ? (
          <p className="text-muted-foreground text-xs">
            <span className="font-medium">Superseded</span>
            {version.reason ? `: ${version.reason}` : ""}
            {version.changed_by ? ` · by ${version.changed_by}` : ""}
          </p>
        ) : null}
      </CardContent>
    </Card>
  );
}

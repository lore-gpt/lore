"use client";

import { useEffect } from "react";

import { addRecentRun } from "./runs-recents";

// Records a run id into the session's recent list — rendered by the trace page only
// once the trace has loaded, so only runs that actually exist are remembered.
export function TrackRecentRun({ runId }: { runId: string }) {
  useEffect(() => {
    addRecentRun(runId);
  }, [runId]);

  return null;
}

"use client";

import { useEffect } from "react";

import { useSearchParams } from "next/navigation";

// Persists the current filtered/paged list URL. A delete must return to the list
// with a HARD navigation (to reset the intercepted modal slot), which would
// otherwise land on the bare, unfiltered list. Reading this back lets the delete
// return to the same filtered/paged view. `deleted` is stripped so it never
// re-triggers the toast.
export const LIST_URL_KEY = "lore_memories_list_url";

export function ListUrlMemory() {
  const sp = useSearchParams();

  useEffect(() => {
    const params = new URLSearchParams(sp);
    params.delete("deleted");
    const qs = params.toString();
    sessionStorage.setItem(LIST_URL_KEY, qs ? `/memories?${qs}` : "/memories");
  }, [sp]);

  return null;
}

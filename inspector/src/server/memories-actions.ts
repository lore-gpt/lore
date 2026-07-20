"use server";

import { headers } from "next/headers";
import { redirect } from "next/navigation";

import { deleteMemory } from "@/lib/api/memories";
import { sanitizeNextPath } from "@/lib/nav";

export type DeleteMemoryState = { ok: true } | { ok: false; error: string } | null;

// Soft-delete a memory and report the outcome. The delete is idempotent per live
// row, so 204 (just deleted) and 404 (already gone / superseded) are both success.
// A 401 routes to the connect flow. On success the CLIENT navigates back to the
// list with a hard navigation — a server redirect here would be a soft navigation,
// which leaves the intercepted-route modal slot showing the now-stale memory.
export async function deleteMemoryAction(_prev: DeleteMemoryState, formData: FormData): Promise<DeleteMemoryState> {
  const id = String(formData.get("id") ?? "").trim();
  if (!id) {
    return { ok: false, error: "Missing memory id." };
  }

  let status: number;
  try {
    status = await deleteMemory(id);
  } catch {
    return { ok: false, error: "Could not reach the Lore server." };
  }

  if (status === 204 || status === 404) {
    return { ok: true };
  }
  if (status === 401) {
    const here = sanitizeNextPath((await headers()).get("x-pathname")) ?? `/memories/${id}`;
    redirect(`/connect?expired=1&next=${encodeURIComponent(here)}`);
  }
  return { ok: false, error: `Delete failed (${status}).` };
}

"use client";

import { useEffect, useRef } from "react";

import { useRouter, useSearchParams } from "next/navigation";

import { toast } from "sonner";

// After a soft-delete the action redirects to /memories?deleted=1. Fire a one-off
// confirmation toast and strip the flag from the URL so a refresh doesn't re-toast.
export function DeleteToast() {
  const sp = useSearchParams();
  const router = useRouter();
  const fired = useRef(false);

  useEffect(() => {
    if (sp.get("deleted") !== "1" || fired.current) {
      return;
    }
    fired.current = true;
    toast.success("Memory soft-deleted.");
    const next = new URLSearchParams(sp);
    next.delete("deleted");
    router.replace(next.toString() ? `/memories?${next}` : "/memories");
  }, [sp, router]);

  return null;
}

"use client";

import { useActionState, useEffect } from "react";

import { Trash2 } from "lucide-react";

import {
  AlertDialog,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogTrigger,
} from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { Spinner } from "@/components/ui/spinner";
import { type DeleteMemoryState, deleteMemoryAction } from "@/server/memories-actions";

import { LIST_URL_KEY } from "../../_components/list-url-memory";

// Confirm-then-soft-delete. On success we return to the list with a HARD
// navigation (window.location) rather than a router push/redirect: the detail can
// be shown inside an intercepted-route modal slot, and only a hard navigation
// resets that slot — a soft navigation would leave the modal open on the deleted
// memory. On failure the dialog stays open with an inline error.
export function DeleteMemoryButton({ id }: { id: string }) {
  const [state, formAction, pending] = useActionState<DeleteMemoryState, FormData>(deleteMemoryAction, null);

  useEffect(() => {
    if (state?.ok) {
      // Return to the same filtered/paged list (persisted by the list) with a HARD
      // navigation, which resets the intercepted modal slot. Falls back to the bare
      // list when nothing was stored (e.g. a deep-linked full page).
      const listUrl = sessionStorage.getItem(LIST_URL_KEY) ?? "/memories";
      const separator = listUrl.includes("?") ? "&" : "?";
      window.location.assign(`${listUrl}${separator}deleted=1`);
    }
  }, [state]);

  const error = state && !state.ok ? state.error : null;

  return (
    <AlertDialog>
      <AlertDialogTrigger asChild>
        <Button variant="destructive" size="sm">
          <Trash2 className="size-4" />
          Delete
        </Button>
      </AlertDialogTrigger>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Soft-delete this memory?</AlertDialogTitle>
          <AlertDialogDescription>
            It drops out of retrieval, packs, and the list immediately, but its version history stays inspectable. This
            is a soft delete, not an erasure.
          </AlertDialogDescription>
        </AlertDialogHeader>
        {error ? <p className="text-destructive text-sm">{error}</p> : null}
        <AlertDialogFooter>
          <AlertDialogCancel disabled={pending}>Cancel</AlertDialogCancel>
          <form action={formAction}>
            <input type="hidden" name="id" value={id} />
            <Button type="submit" variant="destructive" disabled={pending || state?.ok}>
              {pending || state?.ok ? <Spinner className="size-4" /> : <Trash2 className="size-4" />}
              Delete
            </Button>
          </form>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}

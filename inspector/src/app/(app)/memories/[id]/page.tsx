import Link from "next/link";

import { ArrowLeft } from "lucide-react";

import { DetailBody } from "./_components/detail-body";
import { loadMemoryDetail } from "./detail-data";

export const dynamic = "force-dynamic";

// The full page — served on a direct load, refresh, or shared link. A soft
// navigation from the list is intercepted and shown in a slide-over instead.
export default async function MemoryDetailPage({ params }: { params: Promise<{ id: string }> }) {
  const { id } = await params;
  const detail = await loadMemoryDetail(id);

  return (
    <div className="mx-auto flex w-full max-w-3xl flex-col gap-5">
      <Link
        href="/memories"
        className="inline-flex items-center gap-1.5 text-muted-foreground text-sm hover:text-foreground"
      >
        <ArrowLeft className="size-4" />
        Memories
      </Link>
      <DetailBody detail={detail} />
    </div>
  );
}

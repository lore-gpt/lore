import { DetailBody } from "@/app/(app)/memories/[id]/_components/detail-body";
import { DetailSheet } from "@/app/(app)/memories/[id]/_components/detail-sheet";
import { loadMemoryDetail } from "@/app/(app)/memories/[id]/detail-data";

export const dynamic = "force-dynamic";

// Intercepts a soft navigation to /memories/[id] and renders the same detail in a
// right-side slide-over over the list. Shares loadMemoryDetail + DetailBody with
// the full page, so both stay identical.
export default async function MemoryModal({ params }: { params: Promise<{ id: string }> }) {
  const { id } = await params;
  const detail = await loadMemoryDetail(id);

  return (
    <DetailSheet href={`/memories/${id}`}>
      <DetailBody detail={detail} />
    </DetailSheet>
  );
}

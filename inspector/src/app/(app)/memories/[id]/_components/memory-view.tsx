"use client";

import { BadgeCheck, Clock, FileClock, Ghost, Hash, type LucideIcon, ShieldCheck } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import type { Memory, MemoryVersion } from "@/lib/api/types";
import { formatUtc } from "@/lib/format";
import { cn } from "@/lib/utils";

import { CopyButton } from "../../_components/copy-button";
import { DeleteMemoryButton } from "./delete-memory-button";
import { VersionsList } from "./versions-list";

export function MemoryView({
  memory,
  versions,
  versionsLoaded,
}: {
  memory: Memory | null;
  versions: MemoryVersion[];
  versionsLoaded: boolean;
}) {
  if (!memory) {
    return <TombstoneView versions={versions} versionsLoaded={versionsLoaded} />;
  }

  return (
    <div className="flex flex-col gap-5">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex flex-wrap items-center gap-2">
          <Badge>Current</Badge>
          <Badge variant="outline" className="capitalize">
            {memory.kind}
          </Badge>
        </div>
        <DeleteMemoryButton id={memory.id} />
      </div>

      <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
        <Stat icon={ShieldCheck} label="Trust tier" value={memory.trust_tier} />
        <Stat icon={BadgeCheck} label="Review" value={memory.review_status} />
        <Stat icon={Hash} label="Version" value={`v${memory.version}`} />
        <Stat icon={Clock} label="Created" value={formatUtc(memory.created_at)} mono />
      </div>

      <Tabs defaultValue="overview">
        <TabsList>
          <TabsTrigger value="overview">Overview</TabsTrigger>
          <TabsTrigger value="versions">Versions{versionsLoaded ? ` (${versions.length})` : ""}</TabsTrigger>
        </TabsList>

        <TabsContent value="overview" className="flex flex-col gap-4 pt-3">
          <Panel label="Content" copy={memory.content}>
            <p className="whitespace-pre-wrap break-words text-sm">{memory.content}</p>
          </Panel>
          <Panel label="Details">
            <KeyVal label="Created by">{memory.created_by_agent}</KeyVal>
            <KeyVal label="Created" mono>
              {formatUtc(memory.created_at)}
            </KeyVal>
            <KeyVal label="Scope">
              {memory.scope_keys.length > 0 ? (
                <span className="inline-flex flex-wrap justify-end gap-1">
                  {memory.scope_keys.map((key) => (
                    <Badge key={key} variant="secondary" className="font-mono text-xs">
                      {key}
                    </Badge>
                  ))}
                </span>
              ) : (
                "project-wide"
              )}
            </KeyVal>
            <KeyVal label="Memory id" mono copy={memory.id}>
              {memory.id}
            </KeyVal>
            <KeyVal label="Source event" mono>
              {memory.source_event_id ?? "— (non-extracted write)"}
            </KeyVal>
          </Panel>
        </TabsContent>

        <TabsContent value="versions" className="pt-3">
          <VersionSection versions={versions} versionsLoaded={versionsLoaded} />
        </TabsContent>
      </Tabs>
    </div>
  );
}

function Stat({ icon: Icon, label, value, mono }: { icon: LucideIcon; label: string; value: string; mono?: boolean }) {
  return (
    <div className="rounded-lg border bg-muted/30 p-3">
      <div className="flex items-center gap-1.5 text-muted-foreground text-xs">
        <Icon className="size-3.5" />
        {label}
      </div>
      <div className={cn("mt-1 truncate font-medium text-sm", mono ? "font-mono text-xs" : "capitalize")}>{value}</div>
    </div>
  );
}

function Panel({ label, copy, children }: { label: string; copy?: string; children: React.ReactNode }) {
  return (
    <div className="rounded-lg border">
      <div className="flex items-center justify-between border-b px-3 py-2">
        <span className="font-medium text-muted-foreground text-xs">{label}</span>
        {copy ? <CopyButton value={copy} label="" /> : null}
      </div>
      <div className="px-3 py-1">{children}</div>
    </div>
  );
}

function KeyVal({
  label,
  mono,
  copy,
  children,
}: {
  label: string;
  mono?: boolean;
  copy?: string;
  children: React.ReactNode;
}) {
  return (
    <div className="flex items-start justify-between gap-4 border-b py-2 text-sm last:border-0">
      <span className="shrink-0 text-muted-foreground">{label}</span>
      <span className={cn("flex min-w-0 items-center justify-end gap-1 text-right", mono && "font-mono text-xs")}>
        <span className="truncate">{children}</span>
        {copy ? <CopyButton value={copy} label="" /> : null}
      </span>
    </div>
  );
}

function VersionSection({ versions, versionsLoaded }: { versions: MemoryVersion[]; versionsLoaded: boolean }) {
  if (!versionsLoaded) {
    return <p className="text-muted-foreground text-sm">Version history could not be loaded.</p>;
  }
  if (versions.length === 0) {
    return <p className="text-muted-foreground text-sm">No prior versions recorded.</p>;
  }
  return <VersionsList versions={versions} />;
}

function TombstoneView({ versions, versionsLoaded }: { versions: MemoryVersion[]; versionsLoaded: boolean }) {
  return (
    <div className="flex flex-col gap-5">
      <div className="flex items-center gap-3 rounded-lg border border-dashed p-4">
        <div className="flex size-9 shrink-0 items-center justify-center rounded-lg bg-muted text-muted-foreground">
          <Ghost className="size-4" />
        </div>
        <div>
          <p className="font-heading font-medium text-sm">No longer live</p>
          <p className="text-muted-foreground text-sm">
            Soft-deleted or superseded — dropped out of retrieval, packs, and the list. Its history stays inspectable.
          </p>
        </div>
      </div>

      <div className="flex flex-col gap-3">
        <div className="flex items-center gap-2">
          <FileClock className="size-4 text-muted-foreground" />
          <h2 className="font-heading font-medium text-base">Version history</h2>
          {versionsLoaded ? <Badge variant="secondary">{versions.length}</Badge> : null}
        </div>
        <VersionSection versions={versions} versionsLoaded={versionsLoaded} />
      </div>
    </div>
  );
}

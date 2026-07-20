import Link from "next/link";

import { Boxes, Cpu, Database, Layers, type LucideIcon, Route, Server, Tag } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { LORE_API_URL } from "@/lib/api/config";
import { fetchHealth } from "@/lib/api/upstream";
import { cn } from "@/lib/utils";
import { getConnectionState } from "@/server/session";

import { RefreshButton } from "./_components/refresh-button";

export const dynamic = "force-dynamic";

function capitalize(v: string): string {
  return v.length ? v[0].toUpperCase() + v.slice(1) : v;
}

// Map a health token to a word + a color. db/queue/status/workmem all report the
// same vocabulary: ok | degraded | disabled (workmem only).
function statusTone(value: unknown): { word: string; cls: string } {
  const v = String(value ?? "").toLowerCase();
  if (v === "ok" || v === "healthy") {
    return { word: "Healthy", cls: "text-emerald-600 dark:text-emerald-400" };
  }
  if (v === "degraded" || v === "warn") {
    return { word: "Degraded", cls: "text-amber-600 dark:text-amber-400" };
  }
  if (v === "disabled" || v === "off") {
    return { word: "Disabled", cls: "text-muted-foreground" };
  }
  if (v === "") {
    return { word: "Unknown", cls: "text-muted-foreground" };
  }
  return { word: capitalize(v), cls: "text-destructive" };
}

function MetricCard({
  icon: Icon,
  label,
  value,
  valueClass,
  badge,
  subtitle,
}: {
  icon: LucideIcon;
  label: string;
  value: string;
  valueClass?: string;
  badge?: React.ReactNode;
  subtitle: string;
}) {
  return (
    <Card>
      <CardHeader>
        <CardTitle>
          <div className="flex size-7 items-center justify-center rounded-lg border bg-muted text-muted-foreground">
            <Icon className="size-4" />
          </div>
        </CardTitle>
        <CardDescription>{label}</CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-1.5">
        <div className="flex flex-wrap items-center gap-2">
          <div className={cn("font-medium text-2xl leading-none tracking-tight", valueClass)}>{value}</div>
          {badge}
        </div>
        <p className="text-muted-foreground text-sm">{subtitle}</p>
      </CardContent>
    </Card>
  );
}

function DetailRow({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex items-center justify-between gap-4 border-b py-2 last:border-0">
      <span className="text-muted-foreground">{label}</span>
      <span className="min-w-0 truncate text-right">{children}</span>
    </div>
  );
}

export default async function OverviewPage() {
  const [connection, health] = await Promise.all([getConnectionState(), fetchHealth()]);
  const body = health.reachable ? (health.body ?? {}) : {};
  const upstreamHost = LORE_API_URL.replace(/^https?:\/\//, "");

  const dbTone = statusTone(body.db);
  const queueTone = statusTone(body.queue);
  const workmemTone = statusTone(body.workmem);
  const version = typeof body.version === "string" ? body.version : "—";
  const embedder = typeof body.embedder === "string" ? body.embedder : "—";
  const keySource = connection.connected && connection.source === "server" ? "server-configured" : "browser session";

  return (
    <div className="flex flex-col gap-6">
      <div className="flex items-start justify-between gap-4">
        <div>
          <h1 className="font-heading font-semibold text-2xl tracking-tight">Overview</h1>
          <p className="text-muted-foreground text-sm">Connection and live server status for this Lore instance.</p>
        </div>
        <RefreshButton />
      </div>

      {/* KPI strip — one card per health dependency */}
      <div className="grid grid-cols-1 gap-4 *:data-[slot=card]:bg-linear-to-t *:data-[slot=card]:from-primary/5 *:data-[slot=card]:to-card *:data-[slot=card]:shadow-xs sm:grid-cols-2 xl:grid-cols-4 dark:*:data-[slot=card]:bg-card">
        <MetricCard
          icon={Server}
          label="Server"
          value={health.reachable ? "Online" : "Offline"}
          valueClass={health.reachable ? "text-emerald-600 dark:text-emerald-400" : "text-destructive"}
          badge={health.reachable ? <Badge variant="outline">HTTP {health.status}</Badge> : null}
          subtitle={upstreamHost}
        />
        <MetricCard
          icon={Database}
          label="Database"
          value={health.reachable ? dbTone.word : "—"}
          valueClass={health.reachable ? dbTone.cls : "text-muted-foreground"}
          subtitle="PostgreSQL (ParadeDB)"
        />
        <MetricCard
          icon={Layers}
          label="Queue"
          value={health.reachable ? queueTone.word : "—"}
          valueClass={health.reachable ? queueTone.cls : "text-muted-foreground"}
          subtitle="Background jobs"
        />
        <MetricCard icon={Tag} label="Version" value={version} valueClass="font-mono text-xl" subtitle="Server build" />
      </div>

      {/* Detail cards */}
      <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle className="font-heading text-base">Connection</CardTitle>
            <CardDescription>How this Inspector authenticates to the server.</CardDescription>
          </CardHeader>
          <CardContent className="text-sm">
            <DetailRow label="Upstream">
              <span className="font-mono text-xs">{LORE_API_URL}</span>
            </DetailRow>
            <DetailRow label="Key source">{keySource}</DetailRow>
            <DetailRow label="API key">
              {connection.connected ? (
                <span className="font-mono text-xs">{connection.maskedKey}</span>
              ) : (
                "not connected"
              )}
            </DetailRow>
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle className="font-heading text-base">Components</CardTitle>
            <CardDescription>Optional subsystems reported by the server.</CardDescription>
          </CardHeader>
          <CardContent className="text-sm">
            <DetailRow label="Working memory">
              <span
                className={cn(
                  "inline-flex items-center gap-1.5",
                  health.reachable ? workmemTone.cls : "text-muted-foreground",
                )}
              >
                <Boxes className="size-3.5" />
                {health.reachable ? workmemTone.word : "—"}
              </span>
            </DetailRow>
            <DetailRow label="Embedder">
              <span className="inline-flex items-center gap-1.5">
                <Cpu className="size-3.5 text-muted-foreground" />
                <span className="font-mono text-xs">{embedder}</span>
              </span>
            </DetailRow>
          </CardContent>
        </Card>
      </div>

      {/* What's next */}
      <Card>
        <CardHeader>
          <CardTitle className="font-heading text-base">Browsing</CardTitle>
          <CardDescription>Read-only views of what this server has stored.</CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-3 text-muted-foreground text-sm sm:flex-row sm:items-center sm:gap-6">
          <Link href="/memories" className="inline-flex items-center gap-2 hover:text-foreground">
            <Boxes className="size-4" /> Memories browser
          </Link>
          <span className="inline-flex items-center gap-2">
            <Route className="size-4" /> Run traces
            <Badge variant="secondary">soon</Badge>
          </span>
        </CardContent>
      </Card>
    </div>
  );
}

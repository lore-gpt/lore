"use client";

import { KeyRound, LogOut } from "lucide-react";

import { SidebarMenu, SidebarMenuItem } from "@/components/ui/sidebar";
import type { ConnectionState } from "@/server/session";
import { disconnect } from "@/server/session-actions";

// Sidebar footer: shows the active key (masked prefix only) and its source.
// It makes no claim about whether the server is reachable — the overview owns
// the live health check. A cookie session can be disconnected here.
export function ConnectionStatus({ connection }: { connection: ConnectionState }) {
  if (!connection.connected) {
    return (
      <SidebarMenu>
        <SidebarMenuItem className="px-2 py-1.5 text-muted-foreground text-xs">Not connected</SidebarMenuItem>
      </SidebarMenu>
    );
  }

  return (
    <SidebarMenu>
      <SidebarMenuItem className="flex items-center gap-2 rounded-md border bg-sidebar-accent/40 p-2">
        <KeyRound className="size-4 shrink-0 text-muted-foreground" />
        <div className="grid min-w-0 flex-1 leading-tight">
          <span className="truncate font-mono text-xs">{connection.maskedKey}</span>
          <span className="text-[10px] text-muted-foreground">
            {connection.source === "server" ? "server-configured key" : "browser session"}
          </span>
        </div>
        <form action={disconnect}>
          <button
            type="submit"
            aria-label="Log out"
            title="Log out"
            className="text-muted-foreground transition-colors hover:text-foreground"
          >
            <LogOut className="size-4" />
          </button>
        </form>
      </SidebarMenuItem>
    </SidebarMenu>
  );
}

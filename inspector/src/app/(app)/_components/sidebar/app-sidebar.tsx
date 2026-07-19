"use client";

import Link from "next/link";

import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarHeader,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
} from "@/components/ui/sidebar";
import { APP_CONFIG } from "@/config/app-config";
import { sidebarItems } from "@/navigation/sidebar/sidebar-items";
import type { ConnectionState } from "@/server/session";

import { ConnectionStatus } from "./connection-status";
import { NavMain } from "./nav-main";

// The sidebar is always open (not collapsible) per the product decision.
export function AppSidebar({ connection }: { connection: ConnectionState }) {
  return (
    <Sidebar collapsible="none" className="sticky top-0 h-svh border-r">
      <SidebarHeader className="h-14 justify-center border-b">
        <SidebarMenu>
          <SidebarMenuItem>
            <SidebarMenuButton asChild>
              <Link prefetch={false} href="/">
                {/* biome-ignore lint/performance/noImgElement: small static brand mark, not a content image */}
                <img src="/lore-mark.svg" alt="" className="size-6 shrink-0 rounded-md" />
                <span className="font-heading font-semibold text-base">{APP_CONFIG.name}</span>
              </Link>
            </SidebarMenuButton>
          </SidebarMenuItem>
        </SidebarMenu>
      </SidebarHeader>
      <SidebarContent>
        <NavMain items={sidebarItems} />
      </SidebarContent>
      <SidebarFooter>
        <ConnectionStatus connection={connection} />
      </SidebarFooter>
    </Sidebar>
  );
}

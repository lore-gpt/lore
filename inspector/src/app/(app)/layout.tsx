import type { ReactNode } from "react";

import Link from "next/link";
import { redirect } from "next/navigation";

import { AppSidebar } from "@/app/(app)/_components/sidebar/app-sidebar";
import { Button } from "@/components/ui/button";
import { SidebarInset, SidebarProvider } from "@/components/ui/sidebar";
import { APP_CONFIG } from "@/config/app-config";
import { getConnectionState } from "@/server/session";

import { SearchDialog } from "./_components/sidebar/search-dialog";
import { ThemeSwitcher } from "./_components/sidebar/theme-switcher";

function GithubMark(props: React.ComponentProps<"svg">) {
  return (
    <svg viewBox="0 0 24 24" fill="currentColor" {...props}>
      <title>GitHub</title>
      <path d="M12 .297c-6.63 0-12 5.373-12 12 0 5.303 3.438 9.8 8.205 11.385.6.113.82-.258.82-.577 0-.285-.01-1.04-.015-2.04-3.338.724-4.042-1.61-4.042-1.61C4.422 18.07 3.633 17.7 3.633 17.7c-1.087-.744.084-.729.084-.729 1.205.084 1.838 1.236 1.838 1.236 1.07 1.835 2.809 1.305 3.495.998.108-.776.417-1.305.76-1.605-2.665-.3-5.466-1.332-5.466-5.93 0-1.31.465-2.38 1.235-3.22-.135-.303-.54-1.523.105-3.176 0 0 1.005-.322 3.3 1.23.96-.267 1.98-.399 3-.405 1.02.006 2.04.138 3 .405 2.28-1.552 3.285-1.23 3.285-1.23.645 1.653.24 2.873.12 3.176.765.84 1.23 1.91 1.23 3.22 0 4.61-2.805 5.625-5.475 5.92.42.36.81 1.096.81 2.22 0 1.606-.015 2.896-.015 3.286 0 .315.21.69.825.57C20.565 22.092 24 17.592 24 12.297c0-6.627-5.373-12-12-12" />
    </svg>
  );
}

export default async function AppLayout({ children }: Readonly<{ children: ReactNode }>) {
  const connection = await getConnectionState();
  if (!connection.connected) {
    redirect("/connect");
  }

  return (
    <SidebarProvider style={{ "--sidebar-width": "14.5rem" } as React.CSSProperties}>
      <AppSidebar connection={connection} />
      <SidebarInset className="min-w-0 overflow-x-clip">
        <header className="flex h-14 shrink-0 items-center gap-2 border-b px-4 lg:px-6">
          <div className="flex w-full items-center justify-between gap-2">
            <SearchDialog />
            <div className="flex items-center gap-1.5">
              <Button asChild size="icon" aria-label="Lore on GitHub">
                <Link prefetch={false} href={APP_CONFIG.githubUrl} target="_blank" rel="noreferrer">
                  <GithubMark className="size-4" />
                </Link>
              </Button>
              <ThemeSwitcher />
            </div>
          </div>
        </header>
        <div className="min-h-0 min-w-0 flex-1 overflow-x-hidden p-4 md:p-6">{children}</div>
      </SidebarInset>
    </SidebarProvider>
  );
}

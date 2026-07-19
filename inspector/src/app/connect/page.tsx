import { redirect } from "next/navigation";

import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { sanitizeNextPath } from "@/lib/nav";
import { getConnectionState } from "@/server/session";

import { ConnectForm } from "./_components/connect-form";

export const dynamic = "force-dynamic";

export default async function ConnectPage({
  searchParams,
}: {
  searchParams: Promise<{ next?: string; expired?: string }>;
}) {
  const sp = await searchParams;
  const next = sanitizeNextPath(sp.next);
  // `expired=1` means a stored key was just rejected by the server (a 401 on a
  // real request). Show the form so the operator can reconnect instead of
  // bouncing them straight back to the page that failed.
  const expired = sp.expired === "1";

  const connection = await getConnectionState();
  if (connection.connected && !expired) {
    redirect(next ?? "/");
  }

  // When the operator configured a server-side key (LORE_API_KEY) that the server
  // has since rejected, the browser connect form is disabled and would only error
  // on submit — so show an actionable message instead of a dead-end form.
  const serverKeyRejected = expired && connection.connected && connection.source === "server";

  let description: string;
  if (serverKeyRejected) {
    description = "The server-configured key was rejected by the server.";
  } else if (expired) {
    description = "That key was rejected by the server. Reconnect with a valid project API key.";
  } else {
    description = "Connect to a Lore server with a project API key.";
  }

  return (
    <div className="flex min-h-screen items-center justify-center p-6">
      <Card className="w-full max-w-sm">
        <CardHeader className="items-center text-center">
          {/* biome-ignore lint/performance/noImgElement: small static brand mark, not a content image */}
          <img src="/lore-mark.svg" alt="Lore" className="mb-2 size-12 rounded-xl" />
          <CardTitle>Lore Inspector</CardTitle>
          <CardDescription>{description}</CardDescription>
        </CardHeader>
        <CardContent>
          {serverKeyRejected ? (
            <p className="text-muted-foreground text-sm">
              Update <span className="font-mono">LORE_API_KEY</span> with a valid project key and restart the Inspector.
              Browser connect is disabled while a server-side key is configured.
            </p>
          ) : (
            <ConnectForm next={next ?? undefined} />
          )}
        </CardContent>
      </Card>
    </div>
  );
}

import { redirect } from "next/navigation";

import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { getConnectionState } from "@/server/session";

import { ConnectForm } from "./_components/connect-form";

export const dynamic = "force-dynamic";

export default async function ConnectPage() {
  const connection = await getConnectionState();
  if (connection.connected) {
    redirect("/");
  }

  return (
    <div className="flex min-h-screen items-center justify-center p-6">
      <Card className="w-full max-w-sm">
        <CardHeader className="items-center text-center">
          {/* biome-ignore lint/performance/noImgElement: small static brand mark, not a content image */}
          <img src="/lore-mark.svg" alt="Lore" className="mb-2 size-12 rounded-xl" />
          <CardTitle>Lore Inspector</CardTitle>
          <CardDescription>Connect to a Lore server with a project API key.</CardDescription>
        </CardHeader>
        <CardContent>
          <ConnectForm />
        </CardContent>
      </Card>
    </div>
  );
}

import { RecentRuns } from "./_components/recent-runs";
import { RunIdForm } from "./_components/run-id-form";

export default function RunsPage() {
  return (
    <div className="mx-auto flex w-full max-w-2xl flex-col gap-6">
      <div>
        <h1 className="font-heading font-semibold text-2xl tracking-tight">Runs</h1>
        <p className="text-muted-foreground text-sm">
          Inspect a run's context-pack trace. The server doesn't list runs, so paste a run id (from your SDK usage) to
          view its trace.
        </p>
      </div>
      <RunIdForm />
      <RecentRuns />
    </div>
  );
}

import type { components } from "@/generated/openapi";

// Convenience aliases over the spec-generated wire types. Regenerated from
// ../spec/openapi.yaml via `pnpm gen`; do not hand-edit the generated file.
export type Memory = components["schemas"]["Memory"];
export type MemoryListResponse = components["schemas"]["MemoryListResponse"];
export type MemoryVersion = components["schemas"]["MemoryVersion"];
export type MemoryVersionListResponse = components["schemas"]["MemoryVersionListResponse"];
export type RunTraceEntry = components["schemas"]["RunTraceEntry"];
export type RunTraceResponse = components["schemas"]["RunTraceResponse"];
export type Health = components["schemas"]["Health"];
export type ApiErrorBody = components["schemas"]["Error"];

// The wire shapes, aliased from the generated OpenAPI types. This file is the SOLE coupling point between the
// hand-written client and src/generated/openapi.ts: a spec rename or removal breaks the typecheck HERE (a
// second drift signal beyond the git-diff gate), not scattered across the client.
import type { components } from "./generated/openapi.ts";

export type WireCreateEventRequest = components["schemas"]["CreateEventRequest"];
export type WireCreateEventResponse = components["schemas"]["CreateEventResponse"];
export type WireCreateRunResponse = components["schemas"]["CreateRunResponse"];
export type WirePackRequest = components["schemas"]["PackRequest"];
export type WirePackResponse = components["schemas"]["PackResponse"];
export type WirePackSource = components["schemas"]["PackSource"];
export type WireError = components["schemas"]["Error"];

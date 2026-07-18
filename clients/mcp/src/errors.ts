// The errors the REST client throws. Everything is a LoreError; API failures are a discriminated union keyed
// by the server's machine `code`, so a caller switches on `code` and never string-matches a message. This
// hierarchy mirrors the TypeScript SDK's errors.ts one-to-one — a contract-parity test pins the same
// (status, code) -> class mapping in both packages so the two clients can never diverge.
import type { WireError } from "./wire.ts";

/** The machine error codes modelled explicitly. Unmodelled or absent codes surface as {@link UnknownLoreError}. */
export type LoreErrorCode =
  | "invalid_body"
  | "invalid_run_id"
  | "min_seq_out_of_range"
  | "not_found"
  | "unauthorized"
  | "model_mismatch";

/** Base class for everything the REST client throws. Discriminate on `code`. */
export abstract class LoreError extends Error {
  abstract readonly code: LoreErrorCode | "unknown" | "connection" | "parse";
  constructor(message: string, options?: { cause?: unknown }) {
    super(message, options);
    this.name = new.target.name;
  }
}

/** Base for an error the server returned, carrying the HTTP status. */
export abstract class LoreApiError extends LoreError {
  readonly httpStatus: number;
  constructor(message: string, httpStatus: number) {
    super(message);
    this.httpStatus = httpStatus;
  }
}

export class InvalidBodyError extends LoreApiError {
  readonly code = "invalid_body" as const;
}
export class InvalidRunIdError extends LoreApiError {
  readonly code = "invalid_run_id" as const;
}
export class MinSeqOutOfRangeError extends LoreApiError {
  readonly code = "min_seq_out_of_range" as const;
}
export class NotFoundError extends LoreApiError {
  readonly code = "not_found" as const;
}
export class UnauthorizedError extends LoreApiError {
  readonly code = "unauthorized" as const;
}
export class ModelMismatchError extends LoreApiError {
  readonly code = "model_mismatch" as const;
}

/**
 * A server error whose `code` is not modelled — a new code, or an absent one (the API's error schema makes
 * `code` optional). Keyed by HTTP status; the raw code, if any, is on `rawCode`.
 */
export class UnknownLoreError extends LoreApiError {
  readonly code = "unknown" as const;
  readonly rawCode: string | undefined;
  constructor(message: string, httpStatus: number, rawCode?: string) {
    super(message, httpStatus);
    this.rawCode = rawCode;
  }
}

/** The request never reached a response (network failure, timeout, aborted). */
export class LoreConnectionError extends LoreError {
  readonly code = "connection" as const;
  constructor(message: string, options?: { cause?: unknown }) {
    super(message, options);
  }
}

/** The server responded, but the body was not the JSON expected. */
export class LoreParseError extends LoreError {
  readonly code = "parse" as const;
  constructor(message: string, options?: { cause?: unknown }) {
    super(message, options);
  }
}

/** The concrete API-error subclasses a request can throw — the union callers switch on. */
export type LoreApiErrorUnion =
  | InvalidBodyError
  | InvalidRunIdError
  | MinSeqOutOfRangeError
  | NotFoundError
  | UnauthorizedError
  | ModelMismatchError
  | UnknownLoreError;

const KNOWN: Record<LoreErrorCode, (message: string, status: number) => LoreApiErrorUnion> = {
  invalid_body: (m, s) => new InvalidBodyError(m, s),
  invalid_run_id: (m, s) => new InvalidRunIdError(m, s),
  min_seq_out_of_range: (m, s) => new MinSeqOutOfRangeError(m, s),
  not_found: (m, s) => new NotFoundError(m, s),
  unauthorized: (m, s) => new UnauthorizedError(m, s),
  model_mismatch: (m, s) => new ModelMismatchError(m, s),
};

/**
 * Map a server error response (HTTP status + `{code?, message}` body) to the right typed error. A missing or
 * unmodelled code becomes an {@link UnknownLoreError} keyed by status — never a guessed code.
 */
export function fromResponse(status: number, body: WireError): LoreApiErrorUnion {
  const message = body.message || `HTTP ${status}`;
  const raw = body.code;
  if (raw !== undefined && Object.prototype.hasOwnProperty.call(KNOWN, raw)) {
    return KNOWN[raw as LoreErrorCode](message, status);
  }
  return new UnknownLoreError(message, status, raw);
}

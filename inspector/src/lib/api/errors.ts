// A typed error carrying the upstream HTTP status and the API's error `code`
// (e.g. "invalid_cursor", "not_found", "not_connected") so the UI can branch on it.
export class LoreApiError extends Error {
  readonly status: number;
  readonly code?: string;

  constructor(status: number, message: string, code?: string) {
    super(message);
    this.name = "LoreApiError";
    this.status = status;
    this.code = code;
  }
}

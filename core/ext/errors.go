package ext

import "errors"

// ErrPermissionDenied is returned by a PolicyEngine when the granted scopes do
// not permit the requested action. Callers match it with errors.Is.
var ErrPermissionDenied = errors.New("ext: permission denied")

// ErrExtractorUnavailable is returned by an Extractor when the backing model or
// provider could not be reached. The coalesced extraction job treats it as
// retryable. Callers match it with errors.Is.
var ErrExtractorUnavailable = errors.New("ext: extractor unavailable")

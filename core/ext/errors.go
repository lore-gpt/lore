package ext

import "errors"

// ErrPermissionDenied is returned by a PolicyEngine when the granted scopes do
// not permit the requested action. Callers match it with errors.Is.
var ErrPermissionDenied = errors.New("ext: permission denied")

// ErrExtractorUnavailable is returned by an Extractor when the backing model or
// provider could not be reached. The coalesced extraction job treats it as
// retryable. Callers match it with errors.Is.
var ErrExtractorUnavailable = errors.New("ext: extractor unavailable")

// ErrResponseTruncated is returned by an Extractor when the provider answered but the response hit the
// output ceiling, so the distilled result is partial and must not be persisted as complete.
//
// It is deliberately NOT an ErrExtractorUnavailable: the two need opposite responses. An unreachable
// provider is transient and the same window will succeed once it returns, so the right answer is to wait
// and retry unchanged. A truncated response is a property of THIS window — the events are deterministic,
// so retrying them unchanged reproduces the truncation exactly, and a caller that treats it as a transient
// miss burns every attempt on byte-identical input and then discards the job with the run's checkpoint
// frozen. A caller that can shrink its window must be able to tell the two apart, which a shared sentinel
// makes impossible. Callers match it with errors.Is.
var ErrResponseTruncated = errors.New("ext: response truncated at the output ceiling")

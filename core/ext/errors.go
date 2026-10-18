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

// ErrResponseRefused is returned by an Extractor when the provider answered by declining the request —
// a refusal in place of the result — rather than failing to answer at all.
//
// It belongs with ErrResponseTruncated, not ErrExtractorUnavailable, for the same reason: it is a property
// of THIS window. Something in these events is what the model declines, the events are deterministic, and
// a retry of the same window is declined again, so treating it as transient burns every attempt on
// identical input and freezes the run's checkpoint. A caller that can shrink its window can at least
// distil the part of it that is not declined. Callers match it with errors.Is.
var ErrResponseRefused = errors.New("ext: the model declined to process the window")

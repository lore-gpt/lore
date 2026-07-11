package ext

import "errors"

// ErrPermissionDenied is returned by a PolicyEngine when the granted scopes do
// not permit the requested action. Callers match it with errors.Is.
var ErrPermissionDenied = errors.New("ext: permission denied")

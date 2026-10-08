// Package codegraph holds the license texts embedded in every binary, so
// `codegraph licenses` works offline without the release archive's files.
// It lives at the module root because go:embed cannot reach parent
// directories; embedding the root files keeps a single copy.
package codegraph

import _ "embed"

// License is CodeGraph's own license (LICENSE).
//
//go:embed LICENSE
var License string

// ThirdPartyNotices is the notice file for the third-party software linked
// into release binaries (THIRD_PARTY_NOTICES).
//
//go:embed THIRD_PARTY_NOTICES
var ThirdPartyNotices string

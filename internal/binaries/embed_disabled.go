//go:build !embed_binaries

package binaries

// This file is compiled when the `embed_binaries` build tag is NOT set, keeping
// normal `go build` and `go test` small and fast. In this mode the resolver
// relies on a sibling bin/ directory or the system PATH.

// hasEmbedded reports whether binaries were embedded at build time.
func hasEmbedded() bool { return false }

// extractEmbedded is never called in this build mode.
func (r *Resolver) extractEmbedded(t Tool) (string, error) {
	return "", errNoEmbed
}

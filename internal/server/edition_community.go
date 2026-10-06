package server

// edition_community.go names this build. See edition_enterprise.go for why the
// edition is expressed as constants rather than read from a build tag at each
// call site.

// Edition is reported by /health and /api/v1/auth/methods.
const Edition = "community"

const (
	// This build ships no single sign-on provider; it is reported as
	// unavailable rather than simply absent, so that a person looking for it
	// learns where it went. The team boundary is part of both editions.
	editionHasSSO         = false
	editionHasTeamScoping = true
)

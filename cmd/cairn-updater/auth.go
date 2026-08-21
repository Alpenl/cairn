package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

// MinimumDeployTokenLength is the shortest deployment token the helper will
// start with. The socket is only reachable from Caddy, but "only reachable from
// the reverse proxy" is a network fact, and a token short enough to brute force
// through the reverse proxy is a token that only pretends to be a second
// factor.
const MinimumDeployTokenLength = 32

// authenticator checks the single credential the deployment API accepts.
//
// It is deliberately the only credential. A Reader session cookie or an
// installation API token proves something about the application, and the
// entire point of this process is that owning
// the application does not grant the authority to replace it. There is no
// "open mode", no localhost exemption, and no empty-token development path: the
// constructor refuses to build an authenticator without a token, so a
// misconfigured deployment cannot degrade into an unauthenticated one.
type authenticator struct {
	// digest is the SHA-256 of the configured token. Comparing digests rather
	// than raw bytes keeps the comparison length-independent, so a failure
	// cannot leak the token's length through timing.
	digest [sha256.Size]byte
}

func newAuthenticator(token string) *authenticator {
	if token == "" {
		// Unreachable through LoadConfig, which rejects an empty token. Kept as
		// a hard stop so a future caller that builds a Config by hand cannot
		// accidentally create an authenticator that accepts everything.
		panic("cairn-updater: refusing to build an authenticator without a deployment token")
	}
	return &authenticator{digest: sha256.Sum256([]byte(token))}
}

// authorize reports whether the request carries the deployment bearer token.
//
// Anything that is not exactly one `Authorization: Bearer <token>` header is a
// rejection. In particular a Reader session cookie is never consulted, a second
// Authorization header is a rejection rather than a "try them all", and an
// empty bearer value is compared like any other wrong token instead of being
// short-circuited.
func (auth *authenticator) authorize(request *http.Request) bool {
	headers := request.Header.Values("Authorization")
	if len(headers) != 1 {
		return false
	}
	scheme, credential, found := strings.Cut(headers[0], " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	presented := sha256.Sum256([]byte(strings.TrimSpace(credential)))
	return subtle.ConstantTimeCompare(presented[:], auth.digest[:]) == 1
}

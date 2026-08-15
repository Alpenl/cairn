package dto

import "time"

// SessionCreated is the wire response for POST /api/session. The signed token
// is intentionally absent because it is delivered only through an HttpOnly
// cookie.
type SessionCreated struct {
	ExpiresAt              time.Time `json:"expires_at"`
	ClientDataNamespace    string    `json:"client_data_namespace"`
	RepresentationContract string    `json:"representation_contract"`
}

// SessionIdentity is the authoritative identity snapshot returned by
// GET /api/session and embedded in the session-creation response.
type SessionIdentity struct {
	ClientDataNamespace    string `json:"client_data_namespace"`
	RepresentationContract string `json:"representation_contract"`
}

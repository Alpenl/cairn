// Package jsonx is a thin alias over github.com/bytedance/sonic so that
// application code can swap encoding/json without per-call-site changes.
// Sonic is fully drop-in compatible with encoding/json's Marshal/Unmarshal
// surface but is significantly faster on large payloads (analyzer LLM
// responses, yt-dlp metadata, link JSONB columns).
//
// On platforms where sonic does not support JIT it transparently falls back to
// encoding/json via its compat layer, so behaviour is identical across all
// build targets. That fallback is gated by sonic's own build tags:
//
//	//go:build (!amd64 && !arm64) || go1.27 || !go1.17 || (arm64 && !go1.20)
//
// so on this module's go1.26 / amd64+arm64 targets the native path is the one
// actually running. (An earlier version of this comment said the fallback
// starts at go1.26; it starts at go1.27.)
//
// ponytail: this package only earns its keep while the native path is live.
// The moment the module moves to Go 1.27 sonic silently becomes a wrapper
// around encoding/json and the dependency plus this package become pure
// overhead — with no signal that it happened. Re-evaluate at that bump.
// The "significantly faster on large payloads" claim below is also unmeasured:
// there is no benchmark for it in this repo. Benchmark before keeping.
package jsonx

import (
	stdjson "encoding/json"
	"io"

	"github.com/bytedance/sonic"
)

// RawMessage is a type alias for encoding/json.RawMessage so callers that need
// the raw JSON type do not have to import encoding/json themselves.
type RawMessage = stdjson.RawMessage

// api uses ConfigStd to match encoding/json behaviour: HTML escaping on,
// map keys sorted, compact marshaler, string copy, string validation.
var api = sonic.ConfigStd

// Marshal returns the JSON encoding of v.
func Marshal(v any) ([]byte, error) {
	return api.Marshal(v)
}

// MarshalIndent is like Marshal but applies Indent to format the output.
func MarshalIndent(v any, prefix, indent string) ([]byte, error) {
	return api.MarshalIndent(v, prefix, indent)
}

// Unmarshal parses the JSON-encoded data and stores the result in the value
// pointed to by v.
func Unmarshal(data []byte, v any) error {
	return api.Unmarshal(data, v)
}

// NewDecoder returns a Decoder that reads from r.
// The returned Decoder satisfies the sonic.Decoder interface which is a
// strict superset of the methods called by application code (Decode,
// Buffered, DisallowUnknownFields, More, UseNumber).
func NewDecoder(r io.Reader) sonic.Decoder {
	return api.NewDecoder(r)
}

// NewEncoder returns an Encoder that writes to w.
// The returned Encoder satisfies the sonic.Encoder interface which is a
// strict superset of the methods called by application code (Encode,
// SetEscapeHTML, SetIndent).
func NewEncoder(w io.Writer) sonic.Encoder {
	return api.NewEncoder(w)
}

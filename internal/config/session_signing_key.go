package config

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"sync"
)

const ephemeralSessionSigningKeyBytes = 32

type sessionSigningKeyProvider struct {
	once   sync.Once
	source io.Reader
	key    string
	err    error
}

func (p *sessionSigningKeyProvider) keyForProcess() (string, error) {
	p.once.Do(func() {
		random := make([]byte, ephemeralSessionSigningKeyBytes)
		if _, err := io.ReadFull(p.source, random); err != nil {
			p.err = fmt.Errorf("generate ephemeral SESSION_SIGNING_KEY: %w", err)
			return
		}
		p.key = base64.RawURLEncoding.EncodeToString(random)
	})
	return p.key, p.err
}

var processSessionSigningKey = &sessionSigningKeyProvider{source: rand.Reader}

// resolveSessionSigningKey returns the key and whether it is the ephemeral
// per-process fallback. The caller needs that second value: a key nobody
// configured dies with the process, so every restart — including every
// automated update — silently invalidates all Reader sessions, and session
// mode keeps no installation token in the browser to recover with. Silence is
// what makes that expensive: the operator meets it as "the Reader keeps asking
// for the key again" weeks later, with nothing in the log pointing here.
func resolveSessionSigningKey(explicit string) (key string, ephemeral bool, err error) {
	if explicit != "" {
		return explicit, false, nil
	}
	generated, err := processSessionSigningKey.keyForProcess()
	if err != nil {
		return "", false, err
	}
	return generated, true, nil
}

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

func resolveSessionSigningKey(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	return processSessionSigningKey.keyForProcess()
}

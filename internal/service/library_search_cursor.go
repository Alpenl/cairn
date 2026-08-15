package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"webtag/internal/repository"
	"webtag/internal/representation"
)

const (
	libraryThoughtSearchCursorVersion = 1
	libraryThoughtSearchCursorDomain  = "cairn-library-thought-search-cursor-v1\x00"
)

type libraryThoughtSearchCursorPayload struct {
	Version             int    `json:"v"`
	ClientDataNamespace string `json:"client_data_namespace"`
	QueryScope          string `json:"query_scope"`
	RepositoryCursor    string `json:"repository_cursor"`
}

func (s *LibrarySearchService) encodeThoughtSearchCursor(ctx context.Context, query, repositoryCursor string) (string, error) {
	if repositoryCursor == "" {
		return "", nil
	}
	identity, ok := representation.ClientIdentityFromContext(ctx)
	if !ok {
		return "", fmt.Errorf("encode thought search cursor: installation identity unavailable")
	}
	payload, err := json.Marshal(libraryThoughtSearchCursorPayload{
		Version:             libraryThoughtSearchCursorVersion,
		ClientDataNamespace: identity.ClientDataNamespace,
		QueryScope:          libraryThoughtSearchQueryScope(query),
		RepositoryCursor:    repositoryCursor,
	})
	if err != nil {
		return "", fmt.Errorf("encode thought search cursor: %w", err)
	}
	signature := libraryThoughtSearchCursorSignature(s.thoughtCursorKey, payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func (s *LibrarySearchService) decodeThoughtSearchCursor(ctx context.Context, query, token string) (string, error) {
	if strings.TrimSpace(token) == "" {
		return "", nil
	}
	identity, ok := representation.ClientIdentityFromContext(ctx)
	if !ok {
		return "", fmt.Errorf("%w: thought search cursor installation identity", repository.ErrInvalidReaderCursor)
	}
	payload, err := decodeSignedLibraryThoughtSearchCursor(s.thoughtCursorKey, token)
	if err != nil {
		return "", err
	}
	var decoded libraryThoughtSearchCursorPayload
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return "", fmt.Errorf("%w: decode thought search cursor payload", repository.ErrInvalidReaderCursor)
	}
	if decoded.Version != libraryThoughtSearchCursorVersion || decoded.RepositoryCursor == "" ||
		!hmac.Equal([]byte(decoded.ClientDataNamespace), []byte(identity.ClientDataNamespace)) ||
		!hmac.Equal([]byte(decoded.QueryScope), []byte(libraryThoughtSearchQueryScope(query))) {
		return "", fmt.Errorf("%w: thought search cursor binding", repository.ErrInvalidReaderCursor)
	}
	return decoded.RepositoryCursor, nil
}

func decodeSignedLibraryThoughtSearchCursor(key []byte, token string) ([]byte, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf("%w: malformed thought search cursor", repository.ErrInvalidReaderCursor)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || base64.RawURLEncoding.EncodeToString(payload) != parts[0] {
		return nil, fmt.Errorf("%w: decode thought search cursor", repository.ErrInvalidReaderCursor)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || base64.RawURLEncoding.EncodeToString(signature) != parts[1] ||
		!hmac.Equal(signature, libraryThoughtSearchCursorSignature(key, payload)) {
		return nil, fmt.Errorf("%w: thought search cursor signature", repository.ErrInvalidReaderCursor)
	}
	return payload, nil
}

func libraryThoughtSearchQueryScope(query string) string {
	digest := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(query))))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func libraryThoughtSearchCursorSignature(key, payload []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(libraryThoughtSearchCursorDomain))
	_, _ = mac.Write(payload)
	return mac.Sum(nil)[:16]
}

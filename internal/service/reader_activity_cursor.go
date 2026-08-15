package service

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"webtag/internal/model"
	"webtag/internal/repository"
)

const readerActivityCursorVersion = 2

var processReaderCursorKey = func() []byte {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic(fmt.Sprintf("generate Reader cursor key: %v", err))
	}
	return key
}()

type readerActivityCursorPayload struct {
	Version        int    `json:"v"`
	QueryKind      string `json:"query_kind"`
	LastAtUnixNano int64  `json:"last_at_unix_nano"`
	Kind           string `json:"kind"`
	NormalizedKey  string `json:"normalized_key"`
	Key            string `json:"key"`
}

func normalizeReaderActivityKind(raw string) (string, error) {
	kind := strings.ToLower(strings.TrimSpace(raw))
	if kind == "" {
		kind = model.ReaderActivityKindAll
	}
	switch kind {
	case model.ReaderActivityKindAll, model.ReaderActivityKindTag, model.ReaderActivityKindDomain:
		return kind, nil
	default:
		return "", fmt.Errorf("%w: invalid activity query kind", repository.ErrInvalidReaderCursor)
	}
}

func (s *ReaderVNextService) encodeReaderActivityCursor(ctx context.Context, queryKind string, item model.ReaderActivity) string {
	payload, err := json.Marshal(readerActivityCursorPayload{
		Version:        readerActivityCursorVersion,
		QueryKind:      queryKind,
		LastAtUnixNano: item.LastAt.UnixNano(),
		Kind:           item.Kind,
		NormalizedKey:  item.NormalizedKey,
		Key:            item.Key,
	})
	if err != nil {
		return ""
	}
	signature := readerActivityCursorSignature(s.activityCursorKey, payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func (s *ReaderVNextService) decodeReaderActivityCursor(ctx context.Context, queryKind, token string) (*model.ReaderActivityCursor, error) {
	if strings.TrimSpace(token) == "" {
		return nil, nil
	}
	payload, err := decodeSignedReaderActivityCursor(s.activityCursorKey, token)
	if err != nil {
		return nil, err
	}
	var decoded readerActivityCursorPayload
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return nil, fmt.Errorf("%w: decode activity cursor payload", repository.ErrInvalidReaderCursor)
	}
	if err := validateReaderActivityCursorPayload(ctx, queryKind, decoded); err != nil {
		return nil, err
	}
	return &model.ReaderActivityCursor{
		LastAt: time.Unix(0, decoded.LastAtUnixNano).UTC(), Kind: decoded.Kind, NormalizedKey: decoded.NormalizedKey, Key: decoded.Key,
	}, nil
}

func decodeSignedReaderActivityCursor(key []byte, token string) ([]byte, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf("%w: malformed activity cursor", repository.ErrInvalidReaderCursor)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("%w: decode activity cursor", repository.ErrInvalidReaderCursor)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil ||
		base64.RawURLEncoding.EncodeToString(payload) != parts[0] ||
		base64.RawURLEncoding.EncodeToString(signature) != parts[1] ||
		!hmac.Equal(signature, readerActivityCursorSignature(key, payload)) {
		return nil, fmt.Errorf("%w: activity cursor signature", repository.ErrInvalidReaderCursor)
	}
	return payload, nil
}

func validateReaderActivityCursorPayload(_ context.Context, queryKind string, decoded readerActivityCursorPayload) error {
	if decoded.Version != readerActivityCursorVersion || decoded.QueryKind != queryKind {
		return fmt.Errorf("%w: activity cursor binding", repository.ErrInvalidReaderCursor)
	}
	if decoded.Kind != model.ReaderActivityKindTag && decoded.Kind != model.ReaderActivityKindDomain {
		return fmt.Errorf("%w: activity cursor row kind", repository.ErrInvalidReaderCursor)
	}
	if queryKind != model.ReaderActivityKindAll && decoded.Kind != queryKind {
		return fmt.Errorf("%w: activity cursor query kind", repository.ErrInvalidReaderCursor)
	}
	if decoded.LastAtUnixNano <= 0 || decoded.NormalizedKey == "" || decoded.Key == "" {
		return fmt.Errorf("%w: incomplete activity cursor", repository.ErrInvalidReaderCursor)
	}
	return nil
}

func readerActivityCursorSignature(key, payload []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(payload)
	return mac.Sum(nil)[:16]
}

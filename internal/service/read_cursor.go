package service

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"webtag/internal/repository"
)

// cursorHMACPrefixLen 是 HMAC-SHA256 截短保留的字节数。8 字节 (16 hex)
// 仍能提供 ~2^64 的伪造抗性，远超游标作为弱权限边界的实际需求，又把
// token 长度尽量压在 ~70 字符以内，避免分页 URL 过长。
const cursorHMACPrefixLen = 8

// cursorSeparator 把 payload 与 HMAC 段分开。'.' 不在 base64 字母表内，
// 解析时一拆为二即可，不需要长度前缀。
const cursorSeparator = "."

// encodeListCursor serializes a (created_at, id) tuple into an opaque
// base64url token. Keeping the encoding inside the service layer means
// clients treat the token as opaque — we can change the underlying
// scheme (compact binary, signed) without breaking API contracts.
//
// Wave 9 MED M5：当 s.cursorKey 非空时在 payload 后追加
// ".<hex(HMAC-SHA256(payload, key)[:8])>" 防止伪造；为空时落回明文格式
// 保持向后兼容。
func (s *LinkReadService) encodeListCursor(createdAt time.Time, id uuid.UUID) string {
	raw := strconv.FormatInt(createdAt.UTC().UnixNano(), 10) + ":" + id.String()
	if len(s.cursorKey) == 0 {
		return base64.RawURLEncoding.EncodeToString([]byte(raw))
	}
	raw += cursorSeparator + signCursor(s.cursorKey, raw)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeListCursor 是 encodeListCursor 的逆操作；任何解析失败都返回错误，
// 由上层翻成 422，避免把内部格式细节透给客户端。当 s.cursorKey 非空时
// 校验 HMAC：缺失或不匹配都返回 invalid_cursor 错误。
func (s *LinkReadService) decodeListCursor(token string) (*repository.ListLinksCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, fmt.Errorf("decode cursor: %w", err)
	}

	payload := string(raw)
	if len(s.cursorKey) > 0 {
		// 找最后一个 '.'：payload 内部 uuid 字符串本身不含 '.'，所以
		// 第一个 '.' 之后的部分一定是签名段。
		dot := strings.LastIndex(payload, cursorSeparator)
		if dot < 0 {
			return nil, fmt.Errorf("cursor missing signature")
		}
		signed, sig := payload[:dot], payload[dot+1:]
		expected := signCursor(s.cursorKey, signed)
		// hmac.Equal 走常量时间比较防 timing 攻击；token 长度异常会被
		// hmac.Equal 直接判 false（内部先比较长度）。
		if !hmac.Equal([]byte(sig), []byte(expected)) {
			return nil, fmt.Errorf("cursor signature mismatch")
		}
		payload = signed
	}

	parts := strings.SplitN(payload, ":", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("malformed cursor")
	}
	nanos, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("cursor timestamp: %w", err)
	}
	id, err := uuid.Parse(parts[1])
	if err != nil {
		return nil, fmt.Errorf("cursor id: %w", err)
	}
	return &repository.ListLinksCursor{
		CreatedAt: time.Unix(0, nanos).UTC(),
		ID:        id,
	}, nil
}

// signCursor 计算 payload 的 HMAC-SHA256 并截短到 cursorHMACPrefixLen 字节，
// 以 lowercase hex 返回。包装成 helper 是为了 encode/decode 共享同一份字
// 节序，未来若改算法（如改成 BLAKE2 或换截短长度）只需要改这里一处。
func signCursor(key []byte, payload string) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(payload))
	sum := mac.Sum(nil)
	if len(sum) > cursorHMACPrefixLen {
		sum = sum[:cursorHMACPrefixLen]
	}
	return hex.EncodeToString(sum)
}

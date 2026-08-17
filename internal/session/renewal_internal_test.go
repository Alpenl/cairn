package session

import (
	"encoding/base64"
	"strconv"
	"testing"
	"time"
)

// 这组测试锁的是本次修复的行为契约：会话在被使用时会滑动续期，但一次登录的
// 总寿命有硬上限。缺了前者，用户每 12 小时被踢一次且手里没有凭证可恢复
// （session 模式刻意不保存 token）；缺了后者，被窃取的 cookie 只要一直被
// 使用就永不过期。
func TestRenewSlidesTheDeadlineWhenItIsRunningOut(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	claims := Claims{
		ExpiresAt:         now.Add(time.Hour),
		AbsoluteExpiresAt: now.Add(DefaultAbsoluteTTL),
	}
	renewed, ok := Renew(claims, now, DefaultTTL)
	if !ok {
		t.Fatal("a session one hour from expiry must renew")
	}
	if want := now.Add(DefaultTTL); !renewed.ExpiresAt.Equal(want) {
		t.Fatalf("ExpiresAt = %s, want %s", renewed.ExpiresAt, want)
	}
	if !renewed.AbsoluteExpiresAt.Equal(claims.AbsoluteExpiresAt) {
		t.Fatal("renewal must not move the absolute deadline")
	}
}

func TestRenewDeclinesWhileTheDeadlineIsStillFarAway(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	claims := Claims{
		ExpiresAt:         now.Add(DefaultTTL),
		AbsoluteExpiresAt: now.Add(DefaultAbsoluteTTL),
	}
	if _, ok := Renew(claims, now, DefaultTTL); ok {
		t.Fatal("a fresh session must not put a Set-Cookie on every response")
	}
}

// 上限是这次改动里唯一防止「cookie 被偷后永久有效」的东西。
func TestRenewNeverCrossesTheAbsoluteDeadline(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	absolute := now.Add(2 * time.Hour)
	claims := Claims{ExpiresAt: now.Add(time.Hour), AbsoluteExpiresAt: absolute}

	renewed, ok := Renew(claims, now, DefaultTTL)
	if !ok {
		t.Fatal("renewal should still be offered up to the ceiling")
	}
	if !renewed.ExpiresAt.Equal(absolute) {
		t.Fatalf("ExpiresAt = %s, want it clamped to %s", renewed.ExpiresAt, absolute)
	}
	if _, ok := Renew(renewed, now, DefaultTTL); ok {
		t.Fatal("a claim already clamped to the ceiling must not renew again")
	}
}

func TestParseRejectsATokenPastItsAbsoluteDeadline(t *testing.T) {
	key := []byte("absolute-deadline-key")
	now := time.Unix(1_700_000_000, 0)
	// 滑动期限与绝对期限相同的一张票：90 分钟后两者都已过期。正常签发不会
	// 产生这种票，但重放一张旧票时这一层必须拦住。
	token, err := Sign(Claims{
		ExpiresAt:         now.Add(time.Hour),
		AbsoluteExpiresAt: now.Add(time.Hour),
	}, key)
	if err != nil {
		t.Fatalf("Sign() error = %v", err)
	}
	if _, err := Parse(token, key, now.Add(90*time.Minute)); err == nil {
		t.Fatal("a token past both deadlines must not parse")
	}
}

func TestSignRejectsAnAbsoluteDeadlineBeforeTheSlidingOne(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	_, err := Sign(Claims{
		ExpiresAt:         now.Add(2 * time.Hour),
		AbsoluteExpiresAt: now.Add(time.Hour),
	}, []byte("ordering-key"))
	if err == nil {
		t.Fatal("a ceiling earlier than the sliding deadline is incoherent and must not sign")
	}
}

// 部署这次改动时，浏览器里已经存在的是 v3 票。它们必须继续可用直到自然到期
// ——否则这次「修掉被登出」的改动本身就会把所有人登出一次。
func TestParseStillAcceptsLegacyV3Tokens(t *testing.T) {
	key := []byte("legacy-token-key")
	now := time.Unix(1_700_000_000, 0)
	expiresAt := now.Add(time.Hour)
	payload := legacyPayloadVersion + "|" + strconv.FormatInt(expiresAt.Unix(), 10)
	encoded := base64.RawURLEncoding.EncodeToString([]byte(payload))
	token := encoded + "." + base64.RawURLEncoding.EncodeToString(mac(encoded, key))

	claims, err := Parse(token, key, now)
	if err != nil {
		t.Fatalf("Parse(v3) error = %v", err)
	}
	if !claims.AbsoluteExpiresAt.IsZero() {
		t.Fatal("a v3 token carries no absolute deadline")
	}
	if _, ok := Renew(claims, now, DefaultTTL); ok {
		t.Fatal("a v3 token must not renew; it runs out and the next login mints v4")
	}
}

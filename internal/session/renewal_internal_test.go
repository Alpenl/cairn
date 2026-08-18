package session

import (
	"encoding/base64"
	"strconv"
	"testing"
	"time"
)

// 这组测试锁的是滑动续期的行为契约，与两个期限的**具体数值**无关——断言全部
// 用常量表达，正是为了让 DefaultTTL / DefaultAbsoluteTTL 可以按部署策略调整
// 而不必重写这里。缺了滑动续期，用户每过一个 TTL 就被踢一次，且 session 模式
// 刻意不在浏览器保存 token，手里没有凭证可恢复；上限则决定一张票据最长能被
// 续到多久（本部署已将其放宽到实质不再约束，撤回改由轮换签名密钥承担）。
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

// 「登录一次就不再掉线」是这次放宽期限要买到的东西，而它不是任何单个常量的
// 属性——它是 DefaultTTL、renewLeadFraction 与 DefaultAbsoluteTTL 三者共同的
// 结果。把它写成断言，是为了让任何人把其中之一改回保守值时，先坏的是这条
// 测试，而不是几个月后某个用户的登录态：那种失败发生在离改动最远的地方，
// 表现成「Reader 又要重填密钥了」，几乎不可能被归因回常量。
func TestASessionRevisitedWithinTheWindowNeverLapses(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	claims := Claims{
		ExpiresAt:         start.Add(DefaultTTL),
		AbsoluteExpiresAt: start.Add(DefaultAbsoluteTTL),
	}

	// 每两百天回来一次——远长于任何「常用」的定义，却仍在 TTL 之内，这正是
	// 滑动续期应该覆盖的那种使用节奏。连续十次约合五年半。
	const visitEvery = 200 * 24 * time.Hour
	now := start
	for visit := 1; visit <= 10; visit++ {
		now = now.Add(visitEvery)
		if !now.Before(claims.ExpiresAt) {
			t.Fatalf("visit %d: the session lapsed at %s, before the visit at %s",
				visit, claims.ExpiresAt, now)
		}
		renewed, ok := Renew(claims, now, DefaultTTL)
		if !ok {
			t.Fatalf("visit %d: a session %s from expiry must renew", visit, claims.ExpiresAt.Sub(now))
		}
		claims = renewed
	}

	if lived := claims.ExpiresAt.Sub(start); lived < 5*365*24*time.Hour {
		t.Fatalf("one login carried the session %s, want it still alive after five years", lived)
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

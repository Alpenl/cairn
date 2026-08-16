package fetcher

import (
	"strings"
	"testing"
)

// wechatInterstitialTitle / wechatInterstitialBody are the exact strings
// mp.weixin.qq.com served on 2026-08-16, captured from the live captcha
// endpoint through a real browser. Note the title carries no hint at all
// that anything is wrong — only the body does, which is why detection
// keys off body phrases rather than the title.
const (
	wechatInterstitialTitle = "Weixin Official Accounts Platform"
	wechatInterstitialBody  = "环境异常  当前环境异常，完成验证后即可继续访问。  去验证"
)

func TestBlockedInterstitialDetectsWeChatVerification(t *testing.T) {
	if !blockedInterstitial(Content{
		URL:   "https://mp.weixin.qq.com/s/as5t1ZdECbunLbP88t4H2A",
		Title: wechatInterstitialTitle,
		Body:  wechatInterstitialBody,
	}) {
		t.Fatal("WeChat 环境异常 interstitial should be detected")
	}
}

func TestBlockedInterstitialDetectsCaptchaURLRegardlessOfBody(t *testing.T) {
	// Landing on the captcha endpoint means we never saw the article,
	// so the body is irrelevant — including when it is long enough to
	// pass the length gate.
	if !blockedInterstitial(Content{
		URL:  "https://mp.weixin.qq.com/mp/wappoc_appmsgcaptcha?poc_token=x&target_url=y",
		Body: strings.Repeat("正文", 5000),
	}) {
		t.Fatal("captcha endpoint URL should be conclusive on its own")
	}
}

func TestBlockedInterstitialDetectsCloudflareChallenge(t *testing.T) {
	if !blockedInterstitial(Content{
		URL:   "https://example.com/article",
		Title: "Just a moment...",
		Body:  "Enable JavaScript and cookies to continue. Performance & security by Cloudflare",
	}) {
		t.Fatal("Cloudflare challenge should be detected")
	}
}

// The detector must not eat a real article. An essay about WeChat's
// anti-bot flow contains every marker phrase; length is what separates
// it from the interstitial, so this is the false positive that would
// silently discard genuine content.
func TestBlockedInterstitialKeepsLongArticleAboutVerification(t *testing.T) {
	body := "本文分析微信公众号的风控机制。当访问频率过高时，页面会显示环境异常，" +
		"并要求点击去验证完成人机校验。" + strings.Repeat("下面展开讨论其触发条件与规避思路。", 200)
	if blockedInterstitial(Content{
		URL:   "https://example.com/wechat-antibot-analysis",
		Title: "微信风控机制分析",
		Body:  body,
	}) {
		t.Fatal("a long article discussing verification must not be treated as an interstitial")
	}
}

func TestBlockedInterstitialIgnoresOrdinaryContent(t *testing.T) {
	if blockedInterstitial(Content{
		URL:   "https://example.com/post",
		Title: "普通文章",
		Body:  "这是一篇正常的短文，没有任何风控标记。",
	}) {
		t.Fatal("ordinary content must not be treated as an interstitial")
	}
}

// A single incidental marker must not be enough — every phrase in a
// marker set has to be present.
func TestBlockedInterstitialRequiresAllMarkersInASet(t *testing.T) {
	if blockedInterstitial(Content{
		URL:   "https://example.com/post",
		Title: "环境异常检测方法",
		Body:  "本文介绍服务器环境异常的排查步骤。",
	}) {
		t.Fatal("a lone 环境异常 mention must not trigger detection")
	}
}

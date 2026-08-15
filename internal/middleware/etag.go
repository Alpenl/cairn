package middleware

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"

	"webtag/internal/representation"
	"webtag/internal/session"
)

// VersionReader provides one authoritative component-aware representation
// lookup. Errors make conditional handling fail open to the normal handler.
type VersionReader interface {
	Current(context.Context, representation.ComponentSet) (representation.VersionBase, error)
}

// QueryNormalizer maps structured query values to the canonical ETag input.
// eligible=false sends the request through the normal non-cacheable handler.
type QueryNormalizer func(url.Values) (canonical string, eligible bool)

// ConditionalRoutePolicy declares one route's complete version dependencies and
// whether exact matching is safe before the handler proves representation existence.
type ConditionalRoutePolicy struct {
	Components         representation.ComponentSet
	NormalizeQuery     QueryNormalizer
	MatchBeforeHandler bool
}

// ConditionalGetPolicy is an allowlist keyed by Gin's canonical route template.
type ConditionalGetPolicy map[string]ConditionalRoutePolicy

const (
	conditionalCandidateKey = "webtag.conditional.candidate"
	nonCacheableResponseKey = "webtag.conditional.non_cacheable"
)

type conditionalCandidate struct {
	tag         string
	ifNoneMatch string
}

// ConditionalGet prepares version-bound validators for explicitly eligible
// routes. It publishes no cache headers until either an existence-safe exact
// match or a handler-declared successful representation is known.
func ConditionalGet(versions VersionReader, policies ConditionalGetPolicy) gin.HandlerFunc {
	if versions == nil {
		return func(c *gin.Context) { c.Next() }
	}
	return func(c *gin.Context) {
		if c.Request.Method != http.MethodGet {
			c.Next()
			return
		}
		policy, ok := policies[c.FullPath()]
		if !ok || policy.NormalizeQuery == nil || policy.Components.Key() == "" {
			c.Next()
			return
		}
		canonicalQuery, eligible := policy.NormalizeQuery(c.Request.URL.Query())
		if !eligible {
			c.Next()
			return
		}
		clientIdentity, ok := representation.ClientIdentityFromContext(c.Request.Context())
		if !ok {
			c.Next()
			return
		}
		base, err := versions.Current(c.Request.Context(), policy.Components)
		if err != nil || !base.ValidFor(policy.Components) {
			c.Next()
			return
		}
		version, err := representation.NewVersion(base, policy.Components, clientIdentity)
		if err != nil {
			c.Next()
			return
		}
		ctx := representation.WithCacheabilityState(c.Request.Context())
		ctx = representation.WithVersion(ctx, version)
		c.Request = c.Request.WithContext(ctx)
		tag, err := deriveETag(version, canonicalQuery, c.Request.URL.Path)
		if err != nil {
			c.Next()
			return
		}
		candidate := conditionalCandidate{tag: tag, ifNoneMatch: c.GetHeader("If-None-Match")}
		c.Set(conditionalCandidateKey, candidate)

		if canMatchBeforeHandler(policy, candidate) {
			publishConditionalHeaders(c, tag)
			c.AbortWithStatus(http.StatusNotModified)
			return
		}
		c.Next()
	}
}

func canMatchBeforeHandler(policy ConditionalRoutePolicy, candidate conditionalCandidate) bool {
	return policy.MatchBeforeHandler &&
		strings.TrimSpace(candidate.ifNoneMatch) != "*" &&
		matchesETag(candidate.ifNoneMatch, candidate.tag)
}

// CacheableJSON commits a deterministic successful JSON representation. A
// matching wildcard or delayed exact validator becomes 304 only at this point.
func CacheableJSON(c *gin.Context, status int, body any) {
	if status < http.StatusOK || status >= http.StatusMultipleChoices || responseMarkedNonCacheable(c) {
		c.JSON(status, body)
		return
	}
	value, ok := c.Get(conditionalCandidateKey)
	if !ok {
		c.JSON(status, body)
		return
	}
	candidate, ok := value.(conditionalCandidate)
	if !ok || candidate.tag == "" {
		c.JSON(status, body)
		return
	}
	publishConditionalHeaders(c, candidate.tag)
	if matchesSuccessfulRepresentation(candidate.ifNoneMatch, candidate.tag) {
		c.AbortWithStatus(http.StatusNotModified)
		return
	}
	c.JSON(status, body)
}

// MarkResponseNonCacheable opts a fail-soft or partial response out of strong
// validator publication before it is written by CacheableJSON.
func MarkResponseNonCacheable(c *gin.Context) {
	c.Set(nonCacheableResponseKey, true)
}

func responseMarkedNonCacheable(c *gin.Context) bool {
	if representation.ResponseMarkedNonCacheable(c.Request.Context()) {
		return true
	}
	value, ok := c.Get(nonCacheableResponseKey)
	marked, valid := value.(bool)
	return ok && valid && marked
}

func matchesSuccessfulRepresentation(header string, tag string) bool {
	return strings.TrimSpace(header) == "*" || matchesETag(header, tag)
}

func publishConditionalHeaders(c *gin.Context, tag string) {
	header := c.Writer.Header()
	addVary(header, "Authorization")
	addVary(header, "Cookie")
	addVary(header, session.HeaderName)
	header.Set("Cache-Control", "private, no-cache")
	header.Set("ETag", tag)
	if identity, ok := representation.ClientIdentityFromContext(c.Request.Context()); ok {
		header.Set(representation.DataNamespaceHeader, identity.ClientDataNamespace)
	}
}

func addVary(header http.Header, name string) {
	for _, value := range header.Values("Vary") {
		for _, existing := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(existing), name) {
				return
			}
		}
	}
	header.Add("Vary", name)
}

const etagContractVersion = representation.Contract

// deriveETag hashes the contract, actual path, query, and complete canonical
// representation version. The actual URL path is required: c.FullPath() is a
// route template, so using it would make all resource IDs share a validator.
func deriveETag(version representation.RepresentationVersion, canonicalQuery string, route string) (string, error) {
	canonicalVersion, err := version.CanonicalJSON()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(
		etagContractVersion + "\x00" + route + "\x00" + canonicalQuery + "\x00" + string(canonicalVersion),
	))
	return `"` + base64.RawURLEncoding.EncodeToString(sum[:16]) + `"`, nil
}

// matchesETag 按 RFC 9110 §13.1.2 解析 If-None-Match。
//
// 支持逗号分隔的多值列表；比较用弱比较语义（忽略 W/ 前缀）——304 的
// 判定本就允许弱比较。`*` 需要先证明当前表示存在，通用 middleware 无法在
// handler 前完成，因此由调用方在成功选出表示后另行处理。
func matchesETag(header string, tag string) bool {
	header = strings.TrimSpace(header)
	if header == "" {
		return false
	}
	if header == "*" {
		return false
	}
	want := strings.TrimPrefix(tag, "W/")
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if strings.TrimPrefix(candidate, "W/") == want {
			return true
		}
	}
	return false
}

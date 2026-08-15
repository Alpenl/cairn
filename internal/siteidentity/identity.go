// Package siteidentity derives stable, versioned grouping keys for website
// collections. It deliberately owns URL normalisation used by SiteEntry and
// SiteIdentity so the two persistence keys cannot drift over time.
package siteidentity

import (
	"fmt"
	"net"
	"net/url"
	"path"
	"strings"
)

const algorithmVersion = "v1"

// Identity describes the stable grouping target for a collected URL.
// Key is persisted, while Adapter is useful for enforcing narrower rules on
// shared platforms and explaining a grouping decision to callers.
type Identity struct {
	Key           string
	NormalizedURL string
	Host          string
	Adapter       string
}

// FromURL validates and normalizes a public HTTP(S) URL before deriving a
// versioned identity. Generic sites group by host; known shared platforms
// group by their public owner/workspace namespace instead of the entire host.
func FromURL(raw string) (Identity, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return Identity{}, fmt.Errorf("parse site URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return Identity{}, fmt.Errorf("site URL must use http or https")
	}
	if u.User != nil || u.Hostname() == "" {
		return Identity{}, fmt.Errorf("site URL must have a host and no user info")
	}

	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	host = strings.TrimPrefix(host, "www.")
	if ip := net.ParseIP(host); ip != nil {
		return Identity{}, fmt.Errorf("site URL host must be a domain")
	}

	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = host
	u.User = nil
	u.Fragment = ""
	u.RawQuery = normalizeQuery(u.Query()).Encode()
	u.ForceQuery = false
	u.Path = normalizePath(u.Path)
	u.RawPath = ""
	// 端口存在且不是该协议的默认端口时才保留——默认端口写进 host 会让同一站点
	// 产生两个 identity key。
	if port := u.Port(); port != "" &&
		(u.Scheme != "http" || port != "80") &&
		(u.Scheme != "https" || port != "443") {
		u.Host = net.JoinHostPort(host, port)
	}

	adapter, namespace := sharedNamespace(host, u.Path)
	key := algorithmVersion + ":host:" + host
	if adapter != "" {
		key = algorithmVersion + ":" + adapter + ":" + namespace
	}
	return Identity{Key: key, NormalizedURL: u.String(), Host: host, Adapter: adapter}, nil
}

func normalizePath(raw string) string {
	if raw == "" || raw == "/" {
		return ""
	}
	clean := path.Clean("/" + raw)
	if clean == "/" {
		return ""
	}
	return strings.TrimSuffix(clean, "/")
}

func normalizeQuery(values url.Values) url.Values {
	for key := range values {
		if strings.HasPrefix(strings.ToLower(key), "utm_") || strings.EqualFold(key, "fbclid") || strings.EqualFold(key, "gclid") {
			values.Del(key)
		}
	}
	return values
}

func sharedNamespace(host, normalizedPath string) (adapter, namespace string) {
	segments := strings.Split(strings.Trim(normalizedPath, "/"), "/")
	if len(segments) == 0 || segments[0] == "" {
		return "", ""
	}
	switch host {
	case "github.com":
		if len(segments) >= 2 && validSegment(segments[0]) && validSegment(segments[1]) {
			return "github", strings.ToLower(segments[0]) + "/" + strings.ToLower(segments[1])
		}
	case "gitlab.com":
		// GitLab permits nested groups. The stable project delimiter is
		// "-/" for project subresources; absent that delimiter, use the
		// first namespace/project pair as the conservative public default.
		limit := len(segments)
		for i, segment := range segments {
			if segment == "-" {
				limit = i
				break
			}
		}
		if limit >= 2 && validSegment(segments[limit-1]) {
			return "gitlab", strings.ToLower(strings.Join(segments[:limit], "/"))
		}
	case "notion.site", "notion.so", "www.notion.so":
		// A Notion public page is scoped to its first stable path component.
		// The platform hostname itself must never cause unrelated workspaces to
		// merge into one website card.
		if validSegment(segments[0]) {
			return "notion", strings.ToLower(segments[0])
		}
	}
	return "", ""
}

func validSegment(value string) bool {
	return value != "" && len(value) <= 255
}

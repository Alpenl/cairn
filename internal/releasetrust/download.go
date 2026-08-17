package releasetrust

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// Download policy.
//
// The helper never accepts a URL from its caller. It builds asset URLs from the
// exact tag and the fixed repository, and it refuses to follow a redirect that
// leaves the allow-list. GitHub does redirect Release asset downloads to its
// object store, so redirects cannot simply be forbidden — but "follow whatever
// Location says" would turn a compromised or spoofed Release endpoint into an
// arbitrary-URL fetch running as root.
const (
	// ReleaseHost serves the Release page and asset redirects.
	ReleaseHost = "github.com"
	// ReleaseAPIHost serves the Release metadata API.
	ReleaseAPIHost = "api.github.com"
	// MaxDownloadRedirects bounds the redirect chain.
	MaxDownloadRedirects = 5
)

// allowedDownloadHosts is the complete set of hosts a release download may
// touch, including the object stores GitHub redirects asset downloads to.
var allowedDownloadHosts = []string{
	"api.github.com",
	"github.com",
	"objects.githubusercontent.com",
	"release-assets.githubusercontent.com",
}

// AllowedDownloadHosts returns a copy of the allow-list.
func AllowedDownloadHosts() []string {
	return slices.Clone(allowedDownloadHosts)
}

// CheckDownloadURL rejects any URL that is not an https URL on an allowed host.
// Host comparison is exact and case-insensitive: a suffix match would accept
// evil-githubusercontent.com, and a "contains" match would accept
// github.com.attacker.net.
func CheckDownloadURL(candidate *url.URL) error {
	if candidate == nil {
		return errors.New("download URL is missing")
	}
	if candidate.Scheme != "https" {
		return fmt.Errorf("download URL %s is not https", candidate.Redacted())
	}
	if candidate.User != nil {
		return fmt.Errorf("download URL %s carries embedded credentials", candidate.Redacted())
	}
	host := strings.ToLower(candidate.Hostname())
	if !slices.Contains(allowedDownloadHosts, host) {
		return fmt.Errorf("download host %q is not in the release allow-list %v", host, allowedDownloadHosts)
	}
	return nil
}

// CheckDownloadRedirect is the http.Client CheckRedirect policy for release
// downloads. Every hop, not just the final one, has to stay on the allow-list:
// checking only the last URL would let an off-domain hop observe the request
// and hand back its own Location.
func CheckDownloadRedirect(request *http.Request, via []*http.Request) error {
	if len(via) >= MaxDownloadRedirects {
		return fmt.Errorf("release download exceeded %d redirects", MaxDownloadRedirects)
	}
	if err := CheckDownloadURL(request.URL); err != nil {
		return fmt.Errorf("release download redirected off the allow-list: %w", err)
	}
	return nil
}

// AssetURL builds the download URL for one Release asset of an exact tag. The
// helper only ever downloads names it derived itself from the signed manifest,
// so asset names are restricted to plain file names.
func AssetURL(repo, tag, assetName string) (*url.URL, error) {
	if !repoPattern.MatchString(repo) {
		return nil, fmt.Errorf("repository %q is not owner/name", repo)
	}
	if !formalTagPattern.MatchString(tag) {
		return nil, fmt.Errorf("tag %q is not a formal vX.Y.Z release tag", tag)
	}
	if assetName == "" || strings.ContainsAny(assetName, "/\\?#") || strings.Contains(assetName, "..") {
		return nil, fmt.Errorf("asset name %q is not a plain Release asset name", assetName)
	}
	candidate := &url.URL{
		Scheme: "https",
		Host:   ReleaseHost,
		Path:   "/" + repo + "/releases/download/" + tag + "/" + assetName,
	}
	if err := CheckDownloadURL(candidate); err != nil {
		return nil, err
	}
	return candidate, nil
}

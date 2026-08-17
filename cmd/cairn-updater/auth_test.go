package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The permission matrix.
//
// Every route is checked with every credential shape, not just the submit
// route. A read-only version endpoint that leaked without a token would tell an
// unauthenticated caller which release the host runs and whether an update is
// already in flight — reconnaissance for the one endpoint that matters.

func routesUnderTest() []struct {
	method string
	path   string
	body   string
} {
	return []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/api/deploy/system/version", ""},
		{http.MethodGet, "/api/deploy/system/check-updates", ""},
		{http.MethodGet, "/api/deploy/system/check-updates?force=true", ""},
		{http.MethodPost, "/api/deploy/system/jobs", `{"target":"v1.2.3"}`},
		{http.MethodGet, "/api/deploy/system/jobs/0123456789abcdef0123456789abcdef", ""},
	}
}

func TestEveryRouteRefusesEveryCredentialThatIsNotTheDeployToken(t *testing.T) {
	host := newHost(t)

	// Each of these is a credential that exists somewhere in this system and
	// must never be sufficient to deploy.
	rejected := []struct {
		name    string
		headers map[string]string
	}{
		{"no authorization header at all", nil},
		{"an empty bearer value", map[string]string{"Authorization": "Bearer "}},
		{"a bearer with only whitespace", map[string]string{"Authorization": "Bearer    "}},
		{"a wrong token", map[string]string{"Authorization": "Bearer not-the-deploy-token-but-long-enough-to-look"}},
		{"the right token under the wrong scheme", map[string]string{"Authorization": "Token " + testDeployToken}},
		{"basic auth carrying the token", map[string]string{"Authorization": "Basic " + testDeployToken}},
		{"a Reader session cookie", map[string]string{"Cookie": "cairn_session=abcdef0123456789"}},
		{"a Reader session cookie plus an empty bearer", map[string]string{
			"Authorization": "Bearer ", "Cookie": "cairn_session=abcdef0123456789"}},
		{"an admin token in its own header", map[string]string{"X-Admin-Token": testDeployToken}},
		{"an extension token header", map[string]string{"X-Extension-Token": testDeployToken}},
		{"a token in a query-shaped header", map[string]string{"X-Deploy-Token": testDeployToken}},
		{"a prefix of the real token", map[string]string{"Authorization": "Bearer " + testDeployToken[:len(testDeployToken)-1]}},
		{"the real token with a trailing character", map[string]string{"Authorization": "Bearer " + testDeployToken + "x"}},
	}

	for _, route := range routesUnderTest() {
		for _, attempt := range rejected {
			response := host.requestWithHeaders(route.method, route.path, route.body, attempt.headers)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("%s %s with %s was answered %d, expected 401",
					route.method, route.path, attempt.name, response.Code)
			}
			var failure ErrorResponse
			decodeBody(t, response, &failure)
			if failure.Error.Code != CodeUnauthorized {
				t.Fatalf("%s %s with %s produced code %q", route.method, route.path, attempt.name, failure.Error.Code)
			}
			// The rejection must not disclose which part of the credential was
			// wrong, because that is a hint about how to be right.
			if strings.Contains(strings.ToLower(failure.Error.Message), "expired") ||
				strings.Contains(strings.ToLower(failure.Error.Message), "length") {
				t.Fatalf("the rejection message discloses too much: %q", failure.Error.Message)
			}
		}
	}
	if host.serviceWasStopped() {
		t.Fatal("no unauthenticated request may ever reach the service")
	}
}

func TestTwoAuthorizationHeadersAreRejectedRatherThanTried(t *testing.T) {
	host := newHost(t)
	// A proxy that appended a header must not be able to smuggle a valid
	// credential past a client that sent a wrong one, in either order.
	for _, pair := range [][2]string{
		{"Bearer wrong-token-value-that-is-long-enough", "Bearer " + testDeployToken},
		{"Bearer " + testDeployToken, "Bearer wrong-token-value-that-is-long-enough"},
	} {
		request := httptest.NewRequest(http.MethodGet, "/api/deploy/system/version", nil)
		request.Header.Add("Authorization", pair[0])
		request.Header.Add("Authorization", pair[1])
		recorder := httptest.NewRecorder()
		host.server.Handler().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("two Authorization headers were answered %d, expected 401", recorder.Code)
		}
	}
}

func TestTheCorrectTokenIsAcceptedOnEveryRoute(t *testing.T) {
	host := newHost(t)
	for _, route := range routesUnderTest() {
		response := host.request(route.method, route.path, route.body, testDeployToken)
		if response.Code == http.StatusUnauthorized {
			t.Fatalf("%s %s rejected the correct deploy token", route.method, route.path)
		}
	}
	host.server.Wait()
}

func TestTheChallengeNamesBearerSoAClientKnowsTheScheme(t *testing.T) {
	host := newHost(t)
	response := host.request(http.MethodGet, "/api/deploy/system/version", "", "")
	if got := response.Header().Get("WWW-Authenticate"); !strings.HasPrefix(got, "Bearer ") {
		t.Fatalf("expected a Bearer challenge, got %q", got)
	}
}

func TestDeploymentResponsesAreNeverCached(t *testing.T) {
	host := newHost(t)
	response := host.request(http.MethodGet, "/api/deploy/system/version", "", testDeployToken)
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("expected Cache-Control no-store, got %q", got)
	}
}

// TestAnEmptyTokenCannotEvenBeConfigured is the fail-closed half of the
// contract. The application's admin token has a development exemption for an
// empty value; this one must not, in any environment, because the consequence
// of the two is not comparable.
func TestAnEmptyTokenCannotEvenBeConfigured(t *testing.T) {
	base := map[string]string{
		envDatabaseURL: "postgres://webtag@127.0.0.1:5433/webtag",
		envSocketPath:  "/run/cairn-updater.sock",
		envStateDir:    "/var/lib/cairn-updater",
		envCoreDir:     "/opt/webtag",
		envReaderDir:   "/var/www/reader",
		envHelperEnv:   "/etc/cairn-updater.env",
	}
	for _, token := range []string{"", "   ", "\t\n"} {
		environment := map[string]string{}
		for key, value := range base {
			environment[key] = value
		}
		environment[envDeployToken] = token
		if _, err := LoadConfig(lookupFrom(environment)); err == nil {
			t.Fatalf("an empty deploy token %q was accepted", token)
		}
	}
	// A short token is refused too: the socket is proxy-only, but "only the
	// reverse proxy can reach it" is a statement about the network, and the
	// reverse proxy is on the public internet.
	environment := map[string]string{}
	for key, value := range base {
		environment[key] = value
	}
	environment[envDeployToken] = "short"
	if _, err := LoadConfig(lookupFrom(environment)); err == nil {
		t.Fatal("a guessable deploy token was accepted")
	}
}

func TestAValidConfigurationLoads(t *testing.T) {
	environment := map[string]string{
		envDeployToken: testDeployToken,
		envDatabaseURL: "postgres://webtag@127.0.0.1:5433/webtag",
	}
	config, err := LoadConfig(lookupFrom(environment))
	if err != nil {
		t.Fatalf("a complete configuration was rejected: %v", err)
	}
	if config.Repo != Repository {
		t.Fatalf("the repository must be the compiled-in constant, got %q", config.Repo)
	}
	if config.SocketPath != defaultSocketPath {
		t.Fatalf("expected the default socket path, got %q", config.SocketPath)
	}
	// The repository is not settable from the environment.
	environment["CAIRN_UPDATER_REPO"] = "attacker/cairn"
	config, err = LoadConfig(lookupFrom(environment))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if config.Repo != Repository {
		t.Fatalf("the repository must not be settable, got %q", config.Repo)
	}
}

func lookupFrom(environment map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := environment[name]
		return value, ok
	}
}

func (host *host) requestWithHeaders(method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	host.t.Helper()
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	var request *http.Request
	if reader == nil {
		request = httptest.NewRequest(method, path, nil)
	} else {
		request = httptest.NewRequest(method, path, reader)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	host.server.Handler().ServeHTTP(recorder, request)
	return recorder
}

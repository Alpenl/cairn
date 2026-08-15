package app

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

type authenticatedOpenAPIOperation struct {
	Security  *[]map[string][]string                  `json:"security"`
	Responses map[string]authenticatedOpenAPIResponse `json:"responses"`
}

type authenticatedOpenAPIResponse struct {
	Ref     string                     `json:"$ref"`
	Headers map[string]json.RawMessage `json:"headers"`
}

type openAPISecurityScheme struct {
	Type        string `json:"type"`
	Scheme      string `json:"scheme"`
	Description string `json:"description"`
}

func TestAuthenticatedOpenAPIResponsesDeclareNamespaceMarker(t *testing.T) {
	t.Parallel()
	data, err := OpenAPISpec()
	if err != nil {
		t.Fatalf("OpenAPISpec: %v", err)
	}
	var spec struct {
		Security   []map[string][]string                               `json:"security"`
		Paths      map[string]map[string]authenticatedOpenAPIOperation `json:"paths"`
		Components struct {
			Headers   map[string]json.RawMessage              `json:"headers"`
			Responses map[string]authenticatedOpenAPIResponse `json:"responses"`
		} `json:"components"`
	}
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatalf("decode OpenAPI: %v", err)
	}

	wantSecurity := []map[string][]string{
		{"InstallationTokenBearer": {}},
		{"SessionCookie": {}, "SessionCSRFHeader": {}},
		{},
	}
	if _, ok := spec.Components.Headers["WebTagDataNamespace"]; !ok {
		t.Error("components.headers.WebTagDataNamespace is missing")
	}
	for name, response := range spec.Components.Responses {
		if strings.HasPrefix(name, "Authenticated") {
			continue
		}
		if _, ok := response.Headers["X-WebTag-Data-Namespace"]; ok {
			t.Errorf("shared response component %s leaks the authenticated namespace marker", name)
		}
	}

	checked := 0
	for path, item := range spec.Paths {
		for method, operation := range item {
			if !isAuthenticatedPublicAPIOperation(path, method) {
				continue
			}
			checked++
			security := spec.Security
			if operation.Security != nil {
				security = *operation.Security
			}
			if !reflect.DeepEqual(security, wantSecurity) {
				t.Fatalf("%s %s security = %#v, want public API alternatives %#v", strings.ToUpper(method), path, security, wantSecurity)
			}
			for status, response := range operation.Responses {
				if status == "401" {
					continue
				}
				resolved := response
				if response.Ref != "" {
					name := strings.TrimPrefix(response.Ref, "#/components/responses/")
					var ok bool
					resolved, ok = spec.Components.Responses[name]
					if !ok {
						t.Errorf("%s %s response %s references unknown %q", strings.ToUpper(method), path, status, response.Ref)
						continue
					}
				}
				marker, ok := resolved.Headers["X-WebTag-Data-Namespace"]
				if !ok {
					t.Errorf("%s %s response %s omits X-WebTag-Data-Namespace", strings.ToUpper(method), path, status)
					continue
				}
				var ref struct {
					Ref string `json:"$ref"`
				}
				if err := json.Unmarshal(marker, &ref); err != nil || ref.Ref != "#/components/headers/WebTagDataNamespace" {
					t.Errorf("%s %s response %s marker = %s, want WebTagDataNamespace ref", strings.ToUpper(method), path, status, marker)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no authenticated public API operations were checked")
	}
}

func isAuthenticatedPublicAPIOperation(path, method string) bool {
	switch strings.ToLower(method) {
	case strings.ToLower(http.MethodGet), strings.ToLower(http.MethodPost), strings.ToLower(http.MethodPut), strings.ToLower(http.MethodPatch), strings.ToLower(http.MethodDelete):
	default:
		return false
	}
	if !strings.HasPrefix(path, "/api/") || strings.HasPrefix(path, "/api/admin/") {
		return false
	}
	return path != "/api/session" || strings.EqualFold(method, http.MethodGet)
}

func TestAdminOpenAPIOperationsDeclareBearerSecurity(t *testing.T) {
	t.Parallel()
	data, err := OpenAPISpec()
	if err != nil {
		t.Fatalf("OpenAPISpec: %v", err)
	}
	var spec struct {
		Paths      map[string]map[string]authenticatedOpenAPIOperation `json:"paths"`
		Components struct {
			SecuritySchemes map[string]openAPISecurityScheme `json:"securitySchemes"`
		} `json:"components"`
	}
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatalf("decode OpenAPI: %v", err)
	}

	scheme, ok := spec.Components.SecuritySchemes["AdminBearer"]
	if !ok {
		t.Error("components.securitySchemes.AdminBearer is missing")
	} else {
		if scheme.Type != "http" || scheme.Scheme != "bearer" {
			t.Errorf("AdminBearer = %#v, want HTTP bearer", scheme)
		}
		if !strings.Contains(scheme.Description, "ADMIN_AUTH_TOKEN") ||
			!strings.Contains(scheme.Description, "APP_ENV=dev") {
			t.Errorf("AdminBearer description does not document the token and dev-only exemption: %q", scheme.Description)
		}
	}

	wantSecurity := []map[string][]string{{"AdminBearer": {}}}
	checked := 0
	for path, item := range spec.Paths {
		if !strings.HasPrefix(path, "/api/admin/") {
			continue
		}
		for method, operation := range item {
			if !isOpenAPIHTTPMethod(method) {
				continue
			}
			checked++
			if operation.Security == nil {
				t.Errorf("%s %s inherits public API security; want AdminBearer", strings.ToUpper(method), path)
				continue
			}
			if got := *operation.Security; !reflect.DeepEqual(got, wantSecurity) {
				t.Errorf("%s %s security = %#v, want %#v", strings.ToUpper(method), path, got, wantSecurity)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no admin API operations were checked")
	}
}

func isOpenAPIHTTPMethod(method string) bool {
	switch strings.ToLower(method) {
	case strings.ToLower(http.MethodGet), strings.ToLower(http.MethodPost), strings.ToLower(http.MethodPut), strings.ToLower(http.MethodPatch), strings.ToLower(http.MethodDelete):
		return true
	default:
		return false
	}
}

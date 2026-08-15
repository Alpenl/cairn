package app

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"webtag/internal/representation"
)

type sessionOpenAPIOperation struct {
	Security    []map[string][]string             `json:"security"`
	Responses   map[string]sessionOpenAPIResponse `json:"responses"`
	RequestBody struct {
		Content map[string]struct {
			Schema openAPISchema `json:"schema"`
		} `json:"content"`
	} `json:"requestBody"`
}

type sessionOpenAPIResponse struct {
	Headers map[string]json.RawMessage `json:"headers"`
	Content map[string]struct {
		Schema struct {
			Ref string `json:"$ref"`
		} `json:"schema"`
	} `json:"content"`
}

type sessionOpenAPISecurityScheme struct {
	Type   string `json:"type"`
	Scheme string `json:"scheme"`
	In     string `json:"in"`
	Name   string `json:"name"`
}

func TestSessionIdentityOpenAPIContract(t *testing.T) {
	t.Parallel()

	data, err := OpenAPISpec()
	if err != nil {
		t.Fatalf("OpenAPISpec: %v", err)
	}
	var spec struct {
		Paths      map[string]map[string]sessionOpenAPIOperation `json:"paths"`
		Components struct {
			Schemas         map[string]openAPISchema                `json:"schemas"`
			SecuritySchemes map[string]sessionOpenAPISecurityScheme `json:"securitySchemes"`
		} `json:"components"`
	}
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatalf("decode OpenAPI: %v", err)
	}

	path := spec.Paths["/api/session"]
	get, ok := path["get"]
	if !ok {
		t.Fatal("GET /api/session is missing from OpenAPI")
	}
	wantSecurity := []map[string][]string{
		{"InstallationTokenBearer": {}},
		{"SessionCookie": {}, "SessionCSRFHeader": {}},
		{},
	}
	if !reflect.DeepEqual(get.Security, wantSecurity) {
		t.Fatalf("GET identity security = %#v, want exact OR/AND alternatives %#v", get.Security, wantSecurity)
	}
	if got := spec.Components.SecuritySchemes["SessionCSRFHeader"]; got != (sessionOpenAPISecurityScheme{
		Type: "apiKey",
		In:   "header",
		Name: "X-WebTag-Session",
	}) {
		t.Fatalf("SessionCSRFHeader = %#v, want apiKey header X-WebTag-Session", got)
	}
	if got := spec.Components.SecuritySchemes["InstallationTokenBearer"]; got.Type != "http" || got.Scheme != "bearer" {
		t.Fatalf("InstallationTokenBearer = %#v, want HTTP bearer", got)
	}
	for _, retired := range []string{"ApiKeyBearer", "OIDCBearer"} {
		if _, ok := spec.Components.SecuritySchemes[retired]; ok {
			t.Errorf("retired security scheme %s is still published", retired)
		}
	}

	post, ok := path["post"]
	if !ok {
		t.Fatal("POST /api/session is missing from OpenAPI")
	}
	assertSessionOpenAPIResponse(t, get.Responses["200"], "#/components/schemas/SessionIdentity")
	assertSessionOpenAPIResponse(t, post.Responses["201"], "#/components/schemas/SessionCreated")
	assertSessionSchema(t, spec.Components.Schemas, "SessionIdentity", []string{
		"client_data_namespace", "representation_contract",
	})
	assertSessionSchema(t, spec.Components.Schemas, "SessionCreated", []string{
		"client_data_namespace", "expires_at", "representation_contract",
	})
	requestSchema := post.RequestBody.Content["application/json"].Schema
	if !slices.Equal(requestSchema.Required, []string{"token"}) {
		t.Fatalf("POST /api/session required = %v, want [token]", requestSchema.Required)
	}
	if _, ok := requestSchema.Properties["token"]; !ok {
		t.Error("POST /api/session request is missing token")
	}
	if _, ok := requestSchema.Properties["api_key"]; ok {
		t.Error("POST /api/session still publishes retired api_key field")
	}

	contract := spec.Components.Schemas["SessionIdentity"].Properties["representation_contract"]
	var contractShape struct {
		Const string `json:"const"`
	}
	if err := json.Unmarshal(contract, &contractShape); err != nil {
		t.Fatalf("decode representation_contract: %v", err)
	}
	if contractShape.Const != representation.Contract {
		t.Fatalf("representation_contract const = %q, want production contract %q", contractShape.Const, representation.Contract)
	}
}

func assertSessionOpenAPIResponse(t *testing.T, response sessionOpenAPIResponse, wantRef string) {
	t.Helper()
	jsonContent, ok := response.Content["application/json"]
	if !ok || jsonContent.Schema.Ref != wantRef {
		t.Fatalf("response schema = %q, want %q", jsonContent.Schema.Ref, wantRef)
	}
	for _, header := range []string{"X-WebTag-Data-Namespace", "Cache-Control"} {
		if _, ok := response.Headers[header]; !ok {
			t.Errorf("response is missing %s header contract", header)
		}
	}
}

func assertSessionSchema(t *testing.T, schemas map[string]openAPISchema, name string, wantRequired []string) {
	t.Helper()
	schema, ok := schemas[name]
	if !ok {
		t.Fatalf("schema %s is missing", name)
	}
	got := slices.Clone(schema.Required)
	want := slices.Clone(wantRequired)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("%s required = %v, want %v", name, got, want)
	}
	for _, field := range want {
		if _, ok := schema.Properties[field]; !ok {
			t.Errorf("%s property %s is missing", name, field)
		}
	}
	if _, ok := schema.Properties["scopes"]; ok {
		t.Errorf("%s still publishes retired scopes property", name)
	}
}

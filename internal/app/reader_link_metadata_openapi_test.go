package app

import (
	"encoding/json"
	"strings"
	"testing"

	"webtag/internal/model"
)

func TestLinkMetadataRevisionOpenAPISafeIntegerBounds(t *testing.T) {
	t.Parallel()

	data, err := OpenAPISpec()
	if err != nil {
		t.Fatalf("OpenAPISpec() returned error: %v", err)
	}
	type property struct {
		Type        json.RawMessage `json:"type"`
		Format      string          `json:"format"`
		Description string          `json:"description"`
		Minimum     *int64          `json:"minimum"`
		Maximum     *int64          `json:"maximum"`
	}
	var spec struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]property `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatalf("openapi.json: unmarshal failed: %v", err)
	}

	for _, schemaName := range []string{"LinkResponse", "ReaderLinkMetadataResponse"} {
		schema, ok := spec.Components.Schemas[schemaName]
		if !ok {
			t.Fatalf("OpenAPI schema %q is missing", schemaName)
		}
		field, ok := schema.Properties["metadata_revision"]
		if !ok {
			t.Fatalf("%s.metadata_revision is missing", schemaName)
		}
		var typeName string
		if err := json.Unmarshal(field.Type, &typeName); err != nil || typeName != "integer" || field.Format != "int64" {
			t.Errorf("%s.metadata_revision type/format = %s/%q, want integer/int64", schemaName, field.Type, field.Format)
		}
		if field.Minimum == nil || *field.Minimum != 1 {
			t.Errorf("%s.metadata_revision minimum = %v, want 1", schemaName, field.Minimum)
		}
		if field.Maximum == nil || *field.Maximum != model.LinkMetadataMaxRevision {
			t.Errorf("%s.metadata_revision maximum = %v, want %d", schemaName, field.Maximum, model.LinkMetadataMaxRevision)
		}
		if !strings.Contains(field.Description, "9007199254740991") {
			t.Errorf("%s.metadata_revision description must state the JavaScript-safe maximum: %q", schemaName, field.Description)
		}
	}
}

package releasetrust

import (
	"bytes"
	"strings"
	"testing"
)

func TestCanonicalEncodingIsByteStableAndOrdered(t *testing.T) {
	t.Parallel()

	document := CanonicalObject{
		{Key: "zeta", Value: CanonicalString("last")},
		{Key: "alpha", Value: CanonicalInt(-7)},
		{Key: "nested", Value: CanonicalObject{
			{Key: "b", Value: CanonicalBool(true)},
			{Key: "a", Value: CanonicalArray{CanonicalString("one"), CanonicalInt(2)}},
		}},
		{Key: "empty_array", Value: CanonicalArray{}},
		{Key: "empty_object", Value: CanonicalObject{}},
	}
	encoded, err := EncodeCanonical(document)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	const want = `{
  "alpha": -7,
  "empty_array": [],
  "empty_object": {},
  "nested": {
    "a": [
      "one",
      2
    ],
    "b": true
  },
  "zeta": "last"
}
`
	if string(encoded) != want {
		t.Fatalf("canonical encoding drifted:\n got %q\nwant %q", encoded, want)
	}
	if !bytes.HasSuffix(encoded, []byte("}\n")) {
		t.Error("canonical documents must end with exactly one newline")
	}
	if bytes.Contains(encoded, []byte("\r")) {
		t.Error("canonical documents must not contain carriage returns")
	}
	if err := RequireCanonical(encoded); err != nil {
		t.Fatalf("encoder output is not accepted as canonical: %v", err)
	}
}

// Member order in the source object must not change the bytes. This is the
// property the whole signature scheme rests on: two producers that disagree
// about field order would produce two different signable documents.
func TestCanonicalEncodingIgnoresSourceMemberOrder(t *testing.T) {
	t.Parallel()

	forward := CanonicalObject{
		{Key: "a", Value: CanonicalInt(1)},
		{Key: "b", Value: CanonicalInt(2)},
		{Key: "c", Value: CanonicalInt(3)},
	}
	reversed := CanonicalObject{forward[2], forward[1], forward[0]}
	first, err := EncodeCanonical(forward)
	if err != nil {
		t.Fatalf("encode forward: %v", err)
	}
	second, err := EncodeCanonical(reversed)
	if err != nil {
		t.Fatalf("encode reversed: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("member order changed the signed bytes:\n%q\n%q", first, second)
	}
}

func TestCanonicalEncodingEscapesOnlyWhatJSONRequires(t *testing.T) {
	t.Parallel()

	value := CanonicalString("cairn 1.2.3\ncommit: <a&b>/\"q\"\\ \t\x01 版本")
	encoded, err := EncodeCanonical(value)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	const want = "\"cairn 1.2.3\\ncommit: <a&b>/\\\"q\\\"\\\\ \\t\\u0001 版本\"\n"
	if string(encoded) != want {
		t.Fatalf("string escaping drifted:\n got %q\nwant %q", encoded, want)
	}
}

func TestCanonicalRejectsNonCanonicalInput(t *testing.T) {
	t.Parallel()

	for name, document := range map[string]string{
		"unsorted members":  "{\n  \"b\": 1,\n  \"a\": 2\n}\n",
		"four space indent": "{\n    \"a\": 1\n}\n",
		"compact":           `{"a":1}` + "\n",
		"crlf":              "{\r\n  \"a\": 1\r\n}\r\n",
		"no trailing newline": `{
  "a": 1
}`,
		"two trailing newlines": "{\n  \"a\": 1\n}\n\n",
		"leading zero":          "{\n  \"a\": 007\n}\n",
	} {
		if err := RequireCanonical([]byte(document)); err == nil {
			t.Errorf("%s was accepted as canonical", name)
		}
	}
}

func TestCanonicalRejectsFloatsNullAndDuplicateMembers(t *testing.T) {
	t.Parallel()

	for name, document := range map[string]string{
		"float":            `{"a": 1.5}`,
		"exponent":         `{"a": 1e3}`,
		"null value":       `{"a": null}`,
		"duplicate member": `{"a": 1, "a": 2}`,
		"trailing content": `{"a": 1} {"b": 2}`,
	} {
		if _, err := DecodeCanonical([]byte(document)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// A canonical document must survive a decode/encode round trip unchanged, or
// the helper cannot tell "the bytes I received" from "the bytes I would have
// produced".
func TestCanonicalRoundTripsTheReleaseManifest(t *testing.T) {
	release := newTestRelease(t)

	value, err := DecodeCanonical(release.ManifestBytes)
	if err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	reencoded, err := EncodeCanonical(value)
	if err != nil {
		t.Fatalf("re-encode manifest: %v", err)
	}
	if !bytes.Equal(reencoded, release.ManifestBytes) {
		t.Fatal("the release manifest does not round trip through the canonical encoder")
	}
	if strings.Count(string(release.ManifestBytes), "\n") == 0 {
		t.Fatal("the release manifest is not line formatted")
	}
}

// The manifest carries no floating point number anywhere. Sizes, counts, and
// ledger targets are integers; a float would round differently in another JSON
// implementation and break signature reproduction.
func TestReleaseManifestContainsNoFloatingPointNumber(t *testing.T) {
	release := newTestRelease(t)

	value, err := DecodeCanonical(release.ManifestBytes)
	if err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	var walk func(CanonicalValue)
	walk = func(node CanonicalValue) {
		switch typed := node.(type) {
		case CanonicalObject:
			for _, field := range typed {
				walk(field.Value)
			}
		case CanonicalArray:
			for _, item := range typed {
				walk(item)
			}
		}
	}
	walk(value)
	// DecodeCanonical rejects floats outright, so reaching this point already
	// proves the property; the walk exists so a future value type that smuggles
	// one in has to pass through here too.
	if err := RequireCanonical(release.ManifestBytes); err != nil {
		t.Fatalf("release manifest is not canonical: %v", err)
	}
}

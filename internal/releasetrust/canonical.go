package releasetrust

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The canonical encoding. A detached signature covers bytes, not meaning, so
// the manifest needs exactly one legal byte representation. These are the rules
// a re-implementation has to reproduce:
//
//  1. UTF-8, no byte order mark.
//  2. Object member names are sorted by their UTF-8 byte order. Duplicate names
//     are rejected, not last-one-wins.
//  3. Indentation is two spaces per nesting level; the only line terminator is
//     "\n"; the document ends with exactly one "\n".
//  4. Empty objects and arrays are written as "{}" and "[]" on a single line.
//  5. Numbers are 64-bit signed integers written by strconv.FormatInt. There is
//     no floating point value type in this encoder, so "no floats" is a
//     structural property rather than a convention.
//  6. Strings escape only what JSON requires: '"', '\\', and the control
//     characters below U+0020. \b \f \n \r \t are used where they exist, every
//     other control character becomes \u00xx with lowercase hex digits.
//     Non-ASCII text is emitted literally as UTF-8; '<', '>', '&', and '/' are
//     never escaped.
//  7. null is not a legal value anywhere.
//
// RequireCanonical enforces the whole set by re-encoding parsed input and
// comparing bytes, so a producer that gets any rule wrong fails loudly instead
// of producing an unverifiable signature.
const canonicalIndent = "  "

// CanonicalValue is a JSON value that can be written in the canonical encoding.
// The interface is closed by design: the concrete types below are the only
// shapes a manifest may contain, which is what keeps floats and null out.
type CanonicalValue interface {
	writeCanonical(out *bytes.Buffer, depth int) error
}

// CanonicalString is a JSON string.
type CanonicalString string

// CanonicalInt is a JSON integer. There is deliberately no float counterpart.
type CanonicalInt int64

// CanonicalBool is a JSON boolean.
type CanonicalBool bool

// CanonicalArray is an ordered JSON array. Array order is significant and is
// preserved exactly; callers that need a deterministic order must sort before
// building the array.
type CanonicalArray []CanonicalValue

// CanonicalField is one member of a CanonicalObject.
type CanonicalField struct {
	Key   string
	Value CanonicalValue
}

// CanonicalObject is a JSON object. Members may be supplied in any order; the
// encoder sorts them and rejects duplicates.
type CanonicalObject []CanonicalField

func (value CanonicalString) writeCanonical(out *bytes.Buffer, _ int) error {
	return writeCanonicalString(out, string(value))
}

func (value CanonicalInt) writeCanonical(out *bytes.Buffer, _ int) error {
	out.WriteString(strconv.FormatInt(int64(value), 10))
	return nil
}

func (value CanonicalBool) writeCanonical(out *bytes.Buffer, _ int) error {
	if value {
		out.WriteString("true")
	} else {
		out.WriteString("false")
	}
	return nil
}

func (value CanonicalArray) writeCanonical(out *bytes.Buffer, depth int) error {
	if len(value) == 0 {
		out.WriteString("[]")
		return nil
	}
	out.WriteString("[\n")
	for index, item := range value {
		if item == nil {
			return fmt.Errorf("canonical array element %d is null", index)
		}
		writeIndent(out, depth+1)
		if err := item.writeCanonical(out, depth+1); err != nil {
			return err
		}
		if index < len(value)-1 {
			out.WriteByte(',')
		}
		out.WriteByte('\n')
	}
	writeIndent(out, depth)
	out.WriteByte(']')
	return nil
}

func (value CanonicalObject) writeCanonical(out *bytes.Buffer, depth int) error {
	if len(value) == 0 {
		out.WriteString("{}")
		return nil
	}
	fields := slices.Clone(value)
	slices.SortFunc(fields, func(left, right CanonicalField) int {
		return strings.Compare(left.Key, right.Key)
	})
	for index := 1; index < len(fields); index++ {
		if fields[index].Key == fields[index-1].Key {
			return fmt.Errorf("canonical object repeats member %q", fields[index].Key)
		}
	}
	out.WriteString("{\n")
	for index, field := range fields {
		if field.Value == nil {
			return fmt.Errorf("canonical object member %q is null", field.Key)
		}
		writeIndent(out, depth+1)
		if err := writeCanonicalString(out, field.Key); err != nil {
			return err
		}
		out.WriteString(": ")
		if err := field.Value.writeCanonical(out, depth+1); err != nil {
			return err
		}
		if index < len(fields)-1 {
			out.WriteByte(',')
		}
		out.WriteByte('\n')
	}
	writeIndent(out, depth)
	out.WriteByte('}')
	return nil
}

func writeIndent(out *bytes.Buffer, depth int) {
	for i := 0; i < depth; i++ {
		out.WriteString(canonicalIndent)
	}
}

func writeCanonicalString(out *bytes.Buffer, value string) error {
	if !utf8.ValidString(value) {
		return errors.New("canonical strings must be valid UTF-8")
	}
	out.WriteByte('"')
	for _, char := range value {
		switch char {
		case '"':
			out.WriteString(`\"`)
		case '\\':
			out.WriteString(`\\`)
		case '\b':
			out.WriteString(`\b`)
		case '\f':
			out.WriteString(`\f`)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		default:
			if char < 0x20 {
				fmt.Fprintf(out, `\u%04x`, char)
				continue
			}
			out.WriteRune(char)
		}
	}
	out.WriteByte('"')
	return nil
}

// EncodeCanonical renders value in the canonical encoding, trailing newline
// included. The returned bytes are exactly what a detached signature covers.
func EncodeCanonical(value CanonicalValue) ([]byte, error) {
	if value == nil {
		return nil, errors.New("canonical document is null")
	}
	var out bytes.Buffer
	if err := value.writeCanonical(&out, 0); err != nil {
		return nil, err
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// DecodeCanonical parses JSON into the canonical value tree. It rejects null
// and any number that is not an integer, so a document that survives this can
// always be re-encoded canonically.
func DecodeCanonical(data []byte) (CanonicalValue, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := decodeCanonicalValue(decoder)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing content after the JSON document")
	}
	return value, nil
}

func decodeCanonicalValue(decoder *json.Decoder) (CanonicalValue, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("read JSON token: %w", err)
	}
	return decodeCanonicalToken(decoder, token)
}

func decodeCanonicalToken(decoder *json.Decoder, token json.Token) (CanonicalValue, error) {
	switch typed := token.(type) {
	case json.Delim:
		switch typed {
		case '{':
			return decodeCanonicalObject(decoder)
		case '[':
			return decodeCanonicalArray(decoder)
		default:
			return nil, fmt.Errorf("unexpected JSON delimiter %q", typed)
		}
	case string:
		return CanonicalString(typed), nil
	case bool:
		return CanonicalBool(typed), nil
	case json.Number:
		text := typed.String()
		if strings.ContainsAny(text, ".eE") {
			return nil, fmt.Errorf("canonical documents forbid non-integer number %s", text)
		}
		number, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("canonical documents require 64-bit integers: %s", text)
		}
		return CanonicalInt(number), nil
	case nil:
		return nil, errors.New("canonical documents forbid null")
	default:
		return nil, fmt.Errorf("unsupported JSON token %T", token)
	}
}

func decodeCanonicalObject(decoder *json.Decoder) (CanonicalValue, error) {
	object := CanonicalObject{}
	seen := map[string]struct{}{}
	for {
		token, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("read JSON object token: %w", err)
		}
		if delim, ok := token.(json.Delim); ok && delim == '}' {
			return object, nil
		}
		key, ok := token.(string)
		if !ok {
			return nil, fmt.Errorf("object member name is %T, want string", token)
		}
		if _, duplicate := seen[key]; duplicate {
			return nil, fmt.Errorf("object repeats member %q", key)
		}
		seen[key] = struct{}{}
		value, err := decodeCanonicalValue(decoder)
		if err != nil {
			return nil, err
		}
		object = append(object, CanonicalField{Key: key, Value: value})
	}
}

func decodeCanonicalArray(decoder *json.Decoder) (CanonicalValue, error) {
	array := CanonicalArray{}
	for {
		token, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("read JSON array token: %w", err)
		}
		if delim, ok := token.(json.Delim); ok && delim == ']' {
			return array, nil
		}
		value, err := decodeCanonicalToken(decoder, token)
		if err != nil {
			return nil, err
		}
		array = append(array, value)
	}
}

// RequireCanonical reports whether data is already in the canonical encoding.
// The helper must use this rather than re-serialising what it parsed: a
// signature that verifies against a normalised copy of an input the helper did
// not actually receive proves nothing about the bytes on disk.
func RequireCanonical(data []byte) error {
	value, err := DecodeCanonical(data)
	if err != nil {
		return err
	}
	encoded, err := EncodeCanonical(value)
	if err != nil {
		return err
	}
	if !bytes.Equal(encoded, data) {
		return errors.New("document is not in the canonical encoding")
	}
	return nil
}

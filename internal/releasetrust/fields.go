package releasetrust

import (
	"fmt"
	"slices"
	"strings"
)

// fieldReader reads a canonical object member by member and remembers what it
// consumed. requireExhausted then rejects anything left over, so an attacker
// cannot smuggle an extra member past a helper that only reads the ones it
// knows about — the manifest is a closed schema, not a bag of hints.
type fieldReader struct {
	context  string
	object   CanonicalObject
	consumed map[string]struct{}
}

func newFieldReader(object CanonicalObject, context string) *fieldReader {
	return &fieldReader{context: context, object: object, consumed: map[string]struct{}{}}
}

func (reader *fieldReader) value(key string) (CanonicalValue, error) {
	for _, field := range reader.object {
		if field.Key == key {
			reader.consumed[key] = struct{}{}
			return field.Value, nil
		}
	}
	return nil, fmt.Errorf("%s is missing member %q", reader.context, key)
}

func (reader *fieldReader) stringField(key string) (string, error) {
	value, err := reader.value(key)
	if err != nil {
		return "", err
	}
	text, ok := value.(CanonicalString)
	if !ok {
		return "", fmt.Errorf("%s member %q is not a string", reader.context, key)
	}
	return string(text), nil
}

func (reader *fieldReader) int64Field(key string) (int64, error) {
	value, err := reader.value(key)
	if err != nil {
		return 0, err
	}
	number, ok := value.(CanonicalInt)
	if !ok {
		return 0, fmt.Errorf("%s member %q is not an integer", reader.context, key)
	}
	return int64(number), nil
}

func (reader *fieldReader) intField(key string) (int, error) {
	number, err := reader.int64Field(key)
	if err != nil {
		return 0, err
	}
	return int(number), nil
}

func (reader *fieldReader) boolField(key string) (bool, error) {
	value, err := reader.value(key)
	if err != nil {
		return false, err
	}
	flag, ok := value.(CanonicalBool)
	if !ok {
		return false, fmt.Errorf("%s member %q is not a boolean", reader.context, key)
	}
	return bool(flag), nil
}

func (reader *fieldReader) arrayField(key string) (CanonicalArray, error) {
	value, err := reader.value(key)
	if err != nil {
		return nil, err
	}
	array, ok := value.(CanonicalArray)
	if !ok {
		return nil, fmt.Errorf("%s member %q is not an array", reader.context, key)
	}
	return array, nil
}

func (reader *fieldReader) stringArrayField(key string) ([]string, error) {
	array, err := reader.arrayField(key)
	if err != nil {
		return nil, err
	}
	items := make([]string, 0, len(array))
	for index, item := range array {
		text, ok := item.(CanonicalString)
		if !ok {
			return nil, fmt.Errorf("%s member %q element %d is not a string", reader.context, key, index)
		}
		items = append(items, string(text))
	}
	return items, nil
}

func (reader *fieldReader) objectField(key string) (CanonicalObject, error) {
	value, err := reader.value(key)
	if err != nil {
		return nil, err
	}
	return asObject(value, reader.context+"."+key)
}

func (reader *fieldReader) requireExhausted() error {
	var unknown []string
	for _, field := range reader.object {
		if _, used := reader.consumed[field.Key]; !used {
			unknown = append(unknown, field.Key)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	slices.Sort(unknown)
	return fmt.Errorf("%s carries unknown member(s) %s", reader.context, strings.Join(unknown, ", "))
}

func asObject(value CanonicalValue, context string) (CanonicalObject, error) {
	object, ok := value.(CanonicalObject)
	if !ok {
		return nil, fmt.Errorf("%s is not an object", context)
	}
	return object, nil
}

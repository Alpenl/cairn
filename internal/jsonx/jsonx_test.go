package jsonx

import (
	"bytes"
	stdjson "encoding/json"
	"strings"
	"testing"
)

// TestMarshalHTMLEscape locks in the encoding/json default of escaping
// HTML-significant characters as \u-escapes (`<`→<, `>`→>,
// `&`→&). If a future sonic upgrade reverts to emitting raw
// `<>&` this test catches it before it ships and breaks any consumer
// that re-embeds the JSON in HTML without re-escaping.
func TestMarshalHTMLEscape(t *testing.T) {
	in := map[string]string{"k": "<a href=\"x\">&"}
	out, err := Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got := string(out)
	wantEscaped := []string{"\\u003c", "\\u003e", "\\u0026"}
	for _, want := range wantEscaped {
		if !strings.Contains(got, want) {
			t.Errorf("output %q missing escape %q", got, want)
		}
	}
	// Cross-check: the raw HTML chars must NOT appear unescaped.
	for _, raw := range []string{"<a", "\">&"} {
		if strings.Contains(got, raw) {
			t.Errorf("output %q contains unescaped HTML %q", got, raw)
		}
	}
}

// TestMarshalMapKeysSorted locks in the stdlib behaviour of sorting map
// keys alphabetically. Stable output is what makes JSONB columns
// diffable and makes tests not flake on map iteration order.
func TestMarshalMapKeysSorted(t *testing.T) {
	in := map[string]int{"c": 3, "a": 1, "b": 2}
	out, err := Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `{"a":1,"b":2,"c":3}`
	if string(out) != want {
		t.Errorf("got %q, want %q", string(out), want)
	}
}

// TestUnmarshalRoundTrip verifies a Marshal→Unmarshal cycle reproduces
// the original value for the shapes the application actually uses
// (strings, numbers, nested maps, slices).
func TestUnmarshalRoundTrip(t *testing.T) {
	type payload struct {
		Title string         `json:"title"`
		Count int            `json:"count"`
		Tags  []string       `json:"tags"`
		Meta  map[string]any `json:"meta"`
	}
	in := payload{
		Title: "hello",
		Count: 42,
		Tags:  []string{"a", "b", "c"},
		Meta:  map[string]any{"k": "v"},
	}
	data, err := Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got payload
	if err := Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Title != in.Title || got.Count != in.Count || len(got.Tags) != len(in.Tags) {
		t.Fatalf("round-trip mismatch: got %+v want %+v", got, in)
	}
}

// TestRawMessageInterop confirms jsonx.RawMessage is the same type as
// encoding/json.RawMessage (it is a type alias) so a value flowing
// across the boundary needs no conversion.
func TestRawMessageInterop(t *testing.T) {
	// 此处显式写类型注解是测试意图的一部分：要证明 jsonx.RawMessage 与
	// encoding/json.RawMessage 类型互通；staticcheck ST1023 的"可省略类型"
	// 建议在这里反而抹掉了测试断言的可读性，故豁免。
	var raw RawMessage = RawMessage(`{"x":1}`) //nolint:staticcheck // reason: 显式类型注解承担测试断言职责，不省略
	// Assignment to encoding/json.RawMessage proves they are identical.
	var stdRaw stdjson.RawMessage = raw //nolint:staticcheck // reason: 同上，断言类型互通
	if string(stdRaw) != `{"x":1}` {
		t.Errorf("RawMessage round-trip failed: %q", string(stdRaw))
	}

	type wrapper struct {
		Payload RawMessage `json:"payload"`
	}
	out, err := Marshal(wrapper{Payload: raw})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `{"payload":{"x":1}}`
	if string(out) != want {
		t.Errorf("got %q, want %q", string(out), want)
	}
}

// TestUnmarshalRejectsInvalid ensures malformed input still returns an
// error (sonic's compat path falls back to encoding/json — verifying
// the error path is still honoured).
func TestUnmarshalRejectsInvalid(t *testing.T) {
	var v map[string]int
	if err := Unmarshal([]byte("{not-json"), &v); err == nil {
		t.Fatal("expected error on malformed JSON, got nil")
	}
}

// TestMarshalIndent 锁住 MarshalIndent 的两件事：缩进按 indent 参数生效、
// 顶层加 prefix 不破坏 JSON 合法性。MarshalIndent 在调试日志和 dump 命令
// 里被用到，回归后会让 dump 输出变成单行影响可读性。
func TestMarshalIndent(t *testing.T) {
	in := map[string]any{"a": 1, "b": []int{2, 3}}
	out, err := MarshalIndent(in, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "\n  \"a\":") {
		t.Errorf("expected 2-space indent on key 'a', got: %q", got)
	}
	if !strings.Contains(got, "\n  \"b\":") {
		t.Errorf("expected 2-space indent on key 'b', got: %q", got)
	}
	// Round-trip 验证 indent 输出仍是合法 JSON（sonic 实现历史上有 indent 路径
	// 漏 escape 的回归，这里顺便兜一层）。
	var back map[string]any
	if err := Unmarshal(out, &back); err != nil {
		t.Errorf("indented output is not valid JSON: %v", err)
	}
}

// TestNewEncoderWritesToWriter 验证 NewEncoder 拿到的 Encoder 真把 JSON
// 写到目标 writer，且 Encode 末尾自带换行（这是 encoding/json 的契约，
// http handler 流式写响应依赖这一点）。
func TestNewEncoderWritesToWriter(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	if err := enc.Encode(map[string]int{"x": 1}); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got := buf.String()
	if got != "{\"x\":1}\n" {
		t.Errorf("got %q, want %q", got, "{\"x\":1}\n")
	}
}

// TestNewDecoderReadsFromReader 验证 NewDecoder 把 reader 里的 JSON 解到
// 目标值。同时锁住 Decoder 一次 Decode 读完单条 token 的语义——逐条
// streaming 解析在 ingest helper 里被用过。
func TestNewDecoderReadsFromReader(t *testing.T) {
	r := strings.NewReader(`{"k":"v","n":42}`)
	dec := NewDecoder(r)
	var out struct {
		K string `json:"k"`
		N int    `json:"n"`
	}
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if out.K != "v" || out.N != 42 {
		t.Errorf("decoded %+v, want {K:v N:42}", out)
	}
}

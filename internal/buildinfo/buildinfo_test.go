package buildinfo

import (
	"bytes"
	"testing"
)

func TestVersionValueDefaultsToSemverZero(t *testing.T) {
	original := Version
	t.Cleanup(func() {
		Version = original
	})

	Version = ""

	if got := VersionValue(); got != "0.0.0" {
		t.Fatalf("VersionValue() = %q, want %q", got, "0.0.0")
	}
}

func TestCommitValueDefaultsToUnknown(t *testing.T) {
	original := Commit
	t.Cleanup(func() {
		Commit = original
	})

	Commit = ""

	if got := CommitValue(); got != "unknown" {
		t.Fatalf("CommitValue() = %q, want %q", got, "unknown")
	}
}

func TestBuildTimeValueDefaultsToUnknown(t *testing.T) {
	original := BuildTime
	t.Cleanup(func() {
		BuildTime = original
	})

	BuildTime = ""

	if got := BuildTimeValue(); got != "unknown" {
		t.Fatalf("BuildTimeValue() = %q, want %q", got, "unknown")
	}
}

// TestValuesPassThroughWhenInjected 锁住非空分支：-ldflags 注入了真实值
// 时三个 *Value() 必须原样返回，不能错误地走兜底。这一路径是 release
// 构建的实际行为，回归会让 /version 接口和启动日志输出 "0.0.0"/"unknown"。
func TestValuesPassThroughWhenInjected(t *testing.T) {
	origV, origC, origB := Version, Commit, BuildTime
	t.Cleanup(func() {
		Version, Commit, BuildTime = origV, origC, origB
	})

	Version = "1.2.3"
	Commit = "deadbeef"
	BuildTime = "2026-05-18T00:00:00Z"

	if got := VersionValue(); got != "1.2.3" {
		t.Errorf("VersionValue() = %q, want %q", got, "1.2.3")
	}
	if got := CommitValue(); got != "deadbeef" {
		t.Errorf("CommitValue() = %q, want %q", got, "deadbeef")
	}
	if got := BuildTimeValue(); got != "2026-05-18T00:00:00Z" {
		t.Errorf("BuildTimeValue() = %q, want %q", got, "2026-05-18T00:00:00Z")
	}
}

func TestPrintVersionUsesStableCoreIdentity(t *testing.T) {
	origV, origC, origB := Version, Commit, BuildTime
	t.Cleanup(func() {
		Version, Commit, BuildTime = origV, origC, origB
	})
	Version = "1.2.3"
	Commit = "0123456789abcdef"
	BuildTime = "2026-08-14T01:02:03Z"

	var output bytes.Buffer
	handled, err := PrintVersion([]string{"--version"}, &output)
	if err != nil {
		t.Fatalf("PrintVersion() error = %v", err)
	}
	if !handled {
		t.Fatal("PrintVersion() handled = false, want true")
	}
	const want = "cairn 1.2.3\ncommit: 0123456789abcdef\nbuilt: 2026-08-14T01:02:03Z\n"
	if got := output.String(); got != want {
		t.Fatalf("PrintVersion() output = %q, want %q", got, want)
	}
}

func TestPrintVersionIgnoresNonVersionArguments(t *testing.T) {
	var output bytes.Buffer
	handled, err := PrintVersion([]string{"--version", "extra"}, &output)
	if err != nil {
		t.Fatalf("PrintVersion() error = %v", err)
	}
	if handled {
		t.Fatal("PrintVersion() handled = true, want false")
	}
	if output.Len() != 0 {
		t.Fatalf("PrintVersion() wrote %q for an unhandled command", output.String())
	}
}

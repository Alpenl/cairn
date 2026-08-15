// Package buildinfo 暴露构建期通过 -ldflags 注入的版本元信息（版本号、commit、构建时间），
// 供 /version 接口、启动日志与可观测性标签统一引用，避免各处散落硬编码默认值。
package buildinfo

import (
	"fmt"
	"io"
)

// 这三个变量预期由 `go build -ldflags "-X webtag/internal/buildinfo.Version=..."` 在构建时覆写；
// 未注入时（如本地 go run、测试）保留下方的占位默认值，配合 *Value() 的兜底保证输出始终可读。
var (
	// Version 是语义化版本号，构建时由 -ldflags 注入；未注入时为 "0.0.0"。
	Version = "0.0.0"
	// Commit 是源码 Git commit 短哈希，构建时由 -ldflags 注入；未注入时为 "unknown"。
	Commit = "unknown"
	// BuildTime 是构建时间戳（通常为 RFC3339），构建时由 -ldflags 注入；未注入时为 "unknown"。
	BuildTime = "unknown"
)

// VersionValue 返回 Version 的安全读取值，空值兜底为 "0.0.0"，确保日志/接口字段永远非空。
func VersionValue() string {
	if Version == "" {
		return "0.0.0"
	}
	return Version
}

// CommitValue 返回 Commit 的安全读取值，空值兜底为 "unknown"。
func CommitValue() string {
	if Commit == "" {
		return "unknown"
	}
	return Commit
}

// BuildTimeValue 返回 BuildTime 的安全读取值，空值兜底为 "unknown"。
func BuildTimeValue() string {
	if BuildTime == "" {
		return "unknown"
	}
	return BuildTime
}

// Identity is the stable, human-readable Core build identity shared by every
// executable shipped in the same release artifact.
func Identity() string {
	return fmt.Sprintf("cairn %s\ncommit: %s\nbuilt: %s",
		VersionValue(), CommitValue(), BuildTimeValue())
}

// PrintVersion handles the side-effect-free --version path. Callers must run
// it before loading configuration, installing signal handlers, or opening
// external resources.
func PrintVersion(args []string, output io.Writer) (bool, error) {
	if len(args) != 1 || args[0] != "--version" {
		return false, nil
	}
	_, err := fmt.Fprintln(output, Identity())
	return true, err
}

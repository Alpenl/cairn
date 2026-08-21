package fetcher

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"

	"webtag/internal/errsafe"
)

const (
	pdfHelperEnv         = "WEBTAG_PDF_PARSE_HELPER"
	pdfHelperMaxInputEnv = "WEBTAG_PDF_MAX_INPUT"
	pdfHelperMaxCharsEnv = "WEBTAG_PDF_MAX_CHARS"
	pdfHelperMaxPagesEnv = "WEBTAG_PDF_MAX_PAGES"
	pdfHelperMaxObjsEnv  = "WEBTAG_PDF_MAX_OBJECTS"
	pdfHelperMemoryEnv   = "WEBTAG_PDF_MAX_MEMORY"
	pdfHelperCPUEnv      = "WEBTAG_PDF_MAX_CPU_SECONDS"
	pdfHelperTestModeEnv = "WEBTAG_PDF_TEST_MODE"
	pdfHelperTestPathEnv = "WEBTAG_PDF_TEST_ARTIFACT"

	pdfHelperBudgetFailure = 20
	pdfHelperParseFailure  = 21
	pdfHelperTextFailure   = 22
	pdfHelperOutputFailure = 23
	pdfHelperPageLimit     = 24
	pdfHelperOutputLimit   = 25
	pdfHelperInputLimit    = 26
	pdfHelperObjectLimit   = 27
	pdfHelperMemoryLimit   = 28
	pdfHelperCPULimit      = 29
)

type pdfParseOutcome string

const (
	pdfParseOutcomeCrash      pdfParseOutcome = "crash"
	pdfParseOutcomeTimeout    pdfParseOutcome = "timeout"
	pdfParseOutcomeCanceled   pdfParseOutcome = "canceled"
	pdfParseOutcomeParseError pdfParseOutcome = "parse_error"
)

var (
	errPDFResourceBudget = fmt.Errorf("PDF resource budget exceeded: %w", errsafe.ErrParse)
	pdfDeclaredSize      = regexp.MustCompile(`/Size\s+([0-9]+)`)
)

type pdfParseError struct {
	outcome     pdfParseOutcome
	cause       error
	maxRSSBytes int64
}

func (e *pdfParseError) Error() string {
	switch e.outcome {
	case pdfParseOutcomeCrash:
		return "isolated PDF parser crashed: " + errsafe.ErrParse.Error()
	case pdfParseOutcomeTimeout:
		return "isolated PDF parser deadline: " + context.DeadlineExceeded.Error()
	case pdfParseOutcomeCanceled:
		return "isolated PDF parser canceled: " + context.Canceled.Error()
	default:
		return "isolated PDF parser failed: " + errsafe.ErrParse.Error()
	}
}

func (e *pdfParseError) Unwrap() error { return e.cause }

type pdfParseBudget struct {
	maxInputBytes    int64
	maxChars         int
	maxPages         int
	maxObjects       int
	wallTime         time.Duration
	maxMemoryBytes   int64
	maxCPUSeconds    uint64
	testMode         string
	testArtifactPath string
	testTempRoot     string
}

// init turns the current executable into a private parser helper only when the
// parent explicitly marks the environment. In production this re-executes the
// Cairn binary; under go test it re-executes the test binary before TestMain.
func init() {
	if os.Getenv(pdfHelperEnv) != "1" {
		return
	}
	os.Exit(runPDFParserHelper(os.Stdin, os.Stdout))
}

func parsePDFIsolated(ctx context.Context, data []byte, budget pdfParseBudget) (body string, resultErr error) {
	budget = normalizePDFBudget(budget)
	if int64(len(data)) > budget.maxInputBytes {
		return "", errPDFResourceBudget
	}
	if declaredPDFObjectsExceed(data, budget.maxObjects) {
		return "", errPDFResourceBudget
	}
	maxOutputBytes, ok := pdfOutputByteBudget(budget.maxChars)
	if !ok {
		return "", errPDFResourceBudget
	}
	parseCtx, cancel := context.WithTimeout(ctx, budget.wallTime)
	defer cancel()

	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("start isolated PDF parser: %w", errsafe.ErrParse)
	}
	scratchDir, err := os.MkdirTemp(budget.testTempRoot, "cairn-pdf-*")
	if err != nil {
		return "", &pdfParseError{outcome: pdfParseOutcomeParseError, cause: errsafe.ErrParse}
	}
	defer func() {
		if cleanupErr := os.RemoveAll(scratchDir); cleanupErr != nil {
			body = ""
			resultErr = &pdfParseError{outcome: pdfParseOutcomeParseError, cause: errsafe.ErrParse}
		}
	}()
	cmd := exec.CommandContext(parseCtx, executable) //nolint:gosec // executable is the current Cairn binary, not caller-controlled input.
	configurePDFHelperProcess(cmd)
	defer func() {
		if cleanupErr := killPDFHelperProcessGroup(cmd); cleanupErr != nil && !errors.Is(cleanupErr, os.ErrProcessDone) {
			body = ""
			resultErr = &pdfParseError{outcome: pdfParseOutcomeParseError, cause: errsafe.ErrParse}
		}
	}()
	// The parser needs no application configuration. Do not expose provider
	// keys, database URLs, or other parent secrets to a process handling an
	// attacker-controlled document.
	goMemoryLimit := "384MiB"
	if raceDetectorEnabled {
		// Instrumented binaries need substantially more runtime memory. Keep the
		// other parser budgets, but leave heap control to the race runtime.
		goMemoryLimit = "off"
	}
	cmd.Env = []string{
		"GOMAXPROCS=1",
		"GOMEMLIMIT=" + goMemoryLimit,
		pdfHelperEnv + "=1",
		pdfHelperMaxInputEnv + "=" + strconv.FormatInt(budget.maxInputBytes, 10),
		pdfHelperMaxCharsEnv + "=" + strconv.Itoa(budget.maxChars),
		pdfHelperMaxPagesEnv + "=" + strconv.Itoa(budget.maxPages),
		pdfHelperMaxObjsEnv + "=" + strconv.Itoa(budget.maxObjects),
		pdfHelperMemoryEnv + "=" + strconv.FormatInt(budget.maxMemoryBytes, 10),
		pdfHelperCPUEnv + "=" + strconv.FormatUint(budget.maxCPUSeconds, 10),
		"TMPDIR=" + scratchDir,
	}
	if budget.testMode != "" && strings.HasSuffix(executable, ".test") {
		cmd.Env = append(cmd.Env, pdfHelperTestModeEnv+"="+budget.testMode)
		if budget.testArtifactPath != "" {
			cmd.Env = append(cmd.Env, pdfHelperTestPathEnv+"="+budget.testArtifactPath)
		}
	}
	cmd.Stdin = bytes.NewReader(data)
	output := &boundedPDFOutput{remaining: maxOutputBytes + 1}
	cmd.Stdout = output
	cmd.Stderr = io.Discard
	err = cmd.Run()
	if completionErr := classifyPDFHelperCompletion(parseCtx, cmd, output, budget, err); completionErr != nil {
		return "", completionErr
	}
	return limitTextByRunes(output.buf.String(), budget.maxChars), nil
}

func classifyPDFHelperCompletion(parseCtx context.Context, cmd *exec.Cmd, output *boundedPDFOutput, budget pdfParseBudget, runErr error) error {
	if ctxErr := parseCtx.Err(); ctxErr != nil {
		if errors.Is(ctxErr, context.Canceled) {
			return &pdfParseError{outcome: pdfParseOutcomeCanceled, cause: context.Canceled}
		}
		return &pdfParseError{outcome: pdfParseOutcomeTimeout, cause: context.DeadlineExceeded}
	}
	if runErr == nil {
		if output.exceeded {
			return errPDFResourceBudget
		}
		return nil
	}
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) {
		return &pdfParseError{outcome: pdfParseOutcomeParseError, cause: errsafe.ErrParse}
	}
	if protocolErr, ok := pdfHelperProtocolError(exitErr.ExitCode()); ok {
		return protocolErr
	}
	if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() && pdfProcessExhaustedCPU(cmd.ProcessState, budget.maxCPUSeconds) {
		return errPDFResourceBudget
	}
	// Runtime crashes, including hard memory-limit failures, use exit codes
	// outside the helper protocol and remain contained resource failures.
	return &pdfParseError{
		outcome:     pdfParseOutcomeCrash,
		cause:       errPDFResourceBudget,
		maxRSSBytes: pdfProcessMaxRSSBytes(cmd.ProcessState),
	}
}

func pdfHelperProtocolError(exitCode int) (error, bool) {
	switch exitCode {
	case pdfHelperParseFailure, pdfHelperTextFailure, pdfHelperOutputFailure:
		return &pdfParseError{outcome: pdfParseOutcomeParseError, cause: errsafe.ErrParse}, true
	case pdfHelperBudgetFailure:
		return errPDFResourceBudget, true
	case pdfHelperPageLimit, pdfHelperOutputLimit, pdfHelperInputLimit,
		pdfHelperObjectLimit, pdfHelperMemoryLimit, pdfHelperCPULimit:
		return errPDFResourceBudget, true
	default:
		return nil, false
	}
}

func configurePDFHelperProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return killPDFHelperProcessGroup(cmd)
	}
	cmd.WaitDelay = 250 * time.Millisecond
}

func killPDFHelperProcessGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return os.ErrProcessDone
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

func pdfProcessExhaustedCPU(state *os.ProcessState, maxCPUSeconds uint64) bool {
	if state == nil || maxCPUSeconds == 0 {
		return false
	}
	usage, ok := state.SysUsage().(*syscall.Rusage)
	if !ok || usage == nil {
		return false
	}
	usedSeconds := float64(usage.Utime.Sec+usage.Stime.Sec) +
		float64(usage.Utime.Usec+usage.Stime.Usec)/1_000_000
	return usedSeconds >= float64(maxCPUSeconds)-0.1
}

func pdfProcessMaxRSSBytes(state *os.ProcessState) int64 {
	if state == nil {
		return 0
	}
	usage, ok := state.SysUsage().(*syscall.Rusage)
	if !ok || usage == nil || usage.Maxrss <= 0 {
		return 0
	}
	if runtime.GOOS == "darwin" {
		return usage.Maxrss
	}
	return usage.Maxrss * 1024
}

func normalizePDFBudget(budget pdfParseBudget) pdfParseBudget {
	if budget.maxInputBytes <= 0 {
		budget.maxInputBytes = defaultPDFMaxBytes
	}
	if budget.maxChars <= 0 {
		budget.maxChars = defaultPDFMaxChars
	}
	if budget.maxPages <= 0 {
		budget.maxPages = defaultPDFMaxPages
	}
	if budget.maxObjects <= 0 {
		budget.maxObjects = defaultPDFMaxObjects
	}
	if budget.wallTime <= 0 {
		budget.wallTime = defaultPDFParseTimeout
	}
	if budget.maxMemoryBytes <= 0 {
		budget.maxMemoryBytes = defaultPDFMaxMemoryBytes
	}
	if budget.maxCPUSeconds == 0 {
		budget.maxCPUSeconds = defaultPDFMaxCPUSeconds
	}
	return budget
}

type boundedPDFOutput struct {
	buf       bytes.Buffer
	remaining int64
	exceeded  bool
}

func (w *boundedPDFOutput) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		w.exceeded = true
		return 0, errors.New("PDF helper output budget exceeded")
	}
	w.remaining -= int64(len(p))
	return w.buf.Write(p)
}

func runPDFParserHelper(input io.Reader, output io.Writer) int {
	budget := normalizePDFBudget(pdfParseBudget{
		maxInputBytes:  envInt64(pdfHelperMaxInputEnv),
		maxChars:       envInt(pdfHelperMaxCharsEnv),
		maxPages:       envInt(pdfHelperMaxPagesEnv),
		maxObjects:     envInt(pdfHelperMaxObjsEnv),
		maxMemoryBytes: envInt64(pdfHelperMemoryEnv),
	})
	if code := applyPDFHelperResourceLimits(); code != 0 {
		return code
	}
	if handled, code := runPDFParserTestMode(); handled {
		return code
	}
	return parsePDFHelperDocument(input, output, budget)
}

func applyPDFHelperResourceLimits() int {
	memory := envUint64(pdfHelperMemoryEnv)
	if memory == 0 {
		return pdfHelperMemoryLimit
	}
	if !raceDetectorEnabled {
		if err := syscall.Setrlimit(syscall.RLIMIT_DATA, &syscall.Rlimit{Cur: memory, Max: memory}); err != nil {
			return pdfHelperMemoryLimit
		}
	}
	cpu := envUint64(pdfHelperCPUEnv)
	if cpu == 0 {
		return pdfHelperCPULimit
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_CPU, &syscall.Rlimit{Cur: cpu, Max: cpu}); err != nil {
		return pdfHelperCPULimit
	}
	return 0
}

func runPDFParserTestMode() (bool, int) {
	if !strings.HasSuffix(os.Args[0], ".test") {
		return false, 0
	}
	switch os.Getenv(pdfHelperTestModeEnv) {
	case "block":
		blockPDFHelperForTest()
	case "block-ready":
		if !writePDFTestArtifact([]byte("ready")) {
			return true, pdfHelperOutputFailure
		}
		blockPDFHelperForTest()
	case "crash":
		panic("controlled PDF helper crash")
	case "cpu":
		exhaustPDFCPUForTest()
	case "memory":
		exhaustPDFMemoryForTest()
	case "spawn-child":
		return true, runPDFSpawnChildTestMode()
	case "temp-block":
		artifact := filepath.Join(os.TempDir(), "helper-artifact")
		if err := os.WriteFile(artifact, []byte("temporary"), 0o600); err != nil {
			return true, pdfHelperOutputFailure
		}
		blockPDFHelperForTest()
	default:
		return false, 0
	}
	return true, 0
}

func blockPDFHelperForTest() {
	for {
		time.Sleep(time.Hour)
	}
}

func exhaustPDFCPUForTest() {
	for value := uint64(0); ; value++ {
		runtime.KeepAlive(value)
	}
}

func exhaustPDFMemoryForTest() {
	allocations := make([][]byte, 0, 128)
	for {
		block := make([]byte, 4<<20)
		for offset := 0; offset < len(block); offset += 4096 {
			block[offset] = 1
		}
		allocations = append(allocations, block)
		runtime.KeepAlive(allocations)
	}
}

func runPDFSpawnChildTestMode() int {
	child := exec.Command("/bin/sleep", "60") //nolint:gosec // fixed test-only executable and argument
	child.Env = []string{}
	if err := child.Start(); err != nil {
		return pdfHelperBudgetFailure
	}
	if !writePDFTestArtifact([]byte(strconv.Itoa(child.Process.Pid))) {
		_ = child.Process.Kill()
		_ = child.Wait()
		return pdfHelperBudgetFailure
	}
	blockPDFHelperForTest()
	return 0
}

func writePDFTestArtifact(data []byte) bool {
	artifact := os.Getenv(pdfHelperTestPathEnv)
	if artifact == "" {
		return false
	}
	// The path is only forwarded by parsePDFIsolated to a .test executable
	// from an unexported test budget; it is never derived from PDF input.
	return os.WriteFile(artifact, data, 0o600) == nil //nolint:gosec
}

func parsePDFHelperDocument(input io.Reader, output io.Writer, budget pdfParseBudget) int {
	data, err := io.ReadAll(io.LimitReader(input, budget.maxInputBytes+1))
	if err != nil {
		return pdfHelperParseFailure
	}
	if int64(len(data)) > budget.maxInputBytes {
		return pdfHelperInputLimit
	}
	if declaredPDFObjectsExceed(data, budget.maxObjects) {
		return pdfHelperObjectLimit
	}
	reader, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return pdfHelperParseFailure
	}
	pages := reader.NumPage()
	if pages < 0 || pages > budget.maxPages {
		return pdfHelperPageLimit
	}
	plainText, err := reader.GetPlainText()
	if err != nil {
		return pdfHelperTextFailure
	}
	maxOutputBytes, ok := pdfOutputByteBudget(budget.maxChars)
	if !ok {
		return pdfHelperOutputLimit
	}
	text, err := io.ReadAll(io.LimitReader(plainText, maxOutputBytes+1))
	if err != nil {
		return pdfHelperTextFailure
	}
	if int64(len(text)) > maxOutputBytes || utf8.RuneCount(text) > budget.maxChars {
		return pdfHelperOutputLimit
	}
	_, err = output.Write(text)
	if err != nil {
		return pdfHelperOutputFailure
	}
	return 0
}

func pdfOutputByteBudget(maxChars int) (int64, bool) {
	if maxChars <= 0 || int64(maxChars) > (int64(^uint64(0)>>1)-1)/utf8.UTFMax {
		return 0, false
	}
	return int64(maxChars) * utf8.UTFMax, true
}

func declaredPDFObjectsExceed(data []byte, maxObjects int) bool {
	if maxObjects <= 0 {
		return true
	}
	for _, match := range pdfDeclaredSize.FindAllSubmatch(data, -1) {
		if len(match) != 2 {
			continue
		}
		value, err := strconv.ParseUint(string(match[1]), 10, 64)
		if err != nil || value > uint64(maxObjects) {
			return true
		}
	}
	return false
}

func envInt(name string) int {
	value, _ := strconv.Atoi(os.Getenv(name))
	return value
}

func envInt64(name string) int64 {
	value, _ := strconv.ParseInt(os.Getenv(name), 10, 64)
	return value
}

func envUint64(name string) uint64 {
	value, _ := strconv.ParseUint(os.Getenv(name), 10, 64)
	return value
}

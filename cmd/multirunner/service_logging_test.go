package main

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/GerardSmit/multirunner/internal/config"
)

func TestCopyServiceOutputRoutesLinesAndRedactsJSON(t *testing.T) {
	logger := &recordingServiceLogger{}
	copyServiceOutputWithSecrets(strings.NewReader("ready\n{\"jit_config\":\"secret-jit\"}\n"), logger, nil)
	if len(logger.infos) != 2 {
		t.Fatalf("captured lines = %v, want 2", logger.infos)
	}
	logged := strings.Join(logger.infos, "\n")
	if strings.Contains(logged, "secret-jit") {
		t.Fatalf("captured output leaked JIT config: %q", logged)
	}
}

func TestCopyServiceOutputPreservesStructuredLogSeverity(t *testing.T) {
	logger := &recordingServiceLogger{}
	copyServiceOutputWithSecrets(strings.NewReader(strings.Join([]string{
		"time=x level=INFO msg=ready",
		"time=x level=WARN msg=slow",
		`{"level":"ERROR","msg":"failed"}`,
	}, "\n")+"\n"), logger, nil)
	if len(logger.infos) != 1 || len(logger.warnings) != 1 || len(logger.errors) != 1 {
		t.Fatalf("routed info=%v warn=%v error=%v", logger.infos, logger.warnings, logger.errors)
	}
}

func TestCopyServiceOutputRedactsMultilinePrivateKey(t *testing.T) {
	logger := &recordingServiceLogger{}
	input := "before\n-----BEGIN PRIVATE KEY-----\nPRIVATE-BODY\n-----END PRIVATE KEY-----\nafter\n"
	copyServiceOutputWithSecrets(strings.NewReader(input), logger, nil)
	logged := strings.Join(logger.infos, "\n")
	if strings.Contains(logged, "PRIVATE-BODY") || strings.Contains(logged, "BEGIN PRIVATE KEY") {
		t.Fatalf("captured output leaked private key: %q", logged)
	}
	for _, want := range []string{"before", "<redacted-private-key>", "after"} {
		if !strings.Contains(logged, want) {
			t.Errorf("captured output missing %q: %q", want, logged)
		}
	}
}

func TestNewLoggerRedactsConfiguredSecrets(t *testing.T) {
	var output bytes.Buffer
	logger := newLogger(config.Log{Format: "json"}, &output, "literal-pat", "webhook-value")
	logger.Error("request failed",
		slog.String("pat", "literal-pat"),
		slog.Any("err", errors.New("webhook-value was rejected")))
	logged := output.String()
	for _, secret := range []string{"literal-pat", "webhook-value"} {
		if strings.Contains(logged, secret) {
			t.Errorf("structured log leaked %q: %q", secret, logged)
		}
	}
	if !strings.Contains(logged, "<redacted>") {
		t.Fatalf("structured log has no redaction marker: %q", logged)
	}
}

func TestSanitizeLogTextPreservesNonSensitiveContext(t *testing.T) {
	input := "pool=linux runner=build-42 status=failed"
	if got := sanitizeLogText(input); got != input {
		t.Fatalf("sanitizeLogText = %q, want %q", got, input)
	}
}

func TestSanitizeLogTextWithSecretsRedactsLiteralValues(t *testing.T) {
	const secret = "configured-value-without-a-recognizable-format"
	logged := sanitizeLogTextWithSecrets("orchestrator failed: "+secret, secret)
	if strings.Contains(logged, secret) || !strings.Contains(logged, "<redacted>") {
		t.Fatalf("configured secret was not redacted: %q", logged)
	}
}

func TestSanitizeLogTextRemovesTerminalControlSequences(t *testing.T) {
	input := "\x1b[31mfailed\x1b[0m\r\ntag\u202ereversed"
	got := sanitizeLogText(input)
	for _, control := range []string{"\x1b", "\r", "\u202e"} {
		if strings.Contains(got, control) {
			t.Fatalf("sanitized output retains control %q: %q", control, got)
		}
	}
	if !strings.Contains(got, "failed") || !strings.Contains(got, "\n") {
		t.Fatalf("sanitized output lost safe context: %q", got)
	}
}

func TestCaptureServiceOutputLeavesInteractiveStreamsAttached(t *testing.T) {
	restore, err := captureServiceOutput(true, &recordingServiceLogger{})
	if err != nil {
		t.Fatal(err)
	}
	restore()
}

func TestCaptureServiceOutputDoesNotRedirectRawParentStreams(t *testing.T) {
	logger := &recordingServiceLogger{}
	restore, err := captureServiceOutput(false, logger)
	if err != nil {
		t.Fatal(err)
	}
	restore()
	if len(logger.infos)+len(logger.warnings)+len(logger.errors) != 0 {
		t.Fatalf("parent output unexpectedly logged: %+v", logger)
	}
}

func TestCopyServiceOutputBoundsOversizedRecordsAndKeepsDraining(t *testing.T) {
	logger := &recordingServiceLogger{}
	secret := "configured-secret-after-boundary"
	input := strings.Repeat("x", maxServiceLogRecordBytes+serviceLogReadBuffer) +
		" " + secret + "\nafter\n"
	copyServiceOutputWithSecrets(strings.NewReader(input), logger, nil, secret)
	if len(logger.infos) != 2 {
		t.Fatalf("captured lines = %d, want 2", len(logger.infos))
	}
	if len(logger.infos[0]) > maxServiceLogRecordBytes+len(" <truncated>") {
		t.Fatalf("oversized record length = %d", len(logger.infos[0]))
	}
	logged := strings.Join(logger.infos, "\n")
	if strings.Contains(logged, secret) || !strings.Contains(logged, "<truncated>") || !strings.Contains(logged, "after") {
		t.Fatalf("oversized output was not safely drained: %q", logged)
	}
}

func TestCopyServiceOutputTracksPrivateKeyMarkersAcrossReadBoundaries(t *testing.T) {
	logger := &recordingServiceLogger{}
	input := strings.Repeat("x", serviceLogReadBuffer-10) +
		"-----BEGIN PRIVATE KEY-----\nPRIVATE-BODY\n-----END PRIVATE KEY-----\nafter\n"
	copyServiceOutputWithSecrets(strings.NewReader(input), logger, nil)
	logged := strings.Join(logger.infos, "\n")
	for _, secret := range []string{"BEGIN PRIVATE KEY", "PRIVATE-BODY"} {
		if strings.Contains(logged, secret) {
			t.Fatalf("fragmented PEM leaked %q: %q", secret, logged)
		}
	}
	if !strings.Contains(logged, "<redacted-private-key>") || !strings.Contains(logged, "after") {
		t.Fatalf("fragmented PEM lost safe context: %q", logged)
	}
}

func TestSanitizedLogTailIsBoundedAndConcurrent(t *testing.T) {
	tail := &sanitizedLogTail{}
	var writers sync.WaitGroup
	for i := 0; i < 16; i++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			tail.add(strings.Repeat("x", 512))
		}()
	}
	writers.Wait()
	if got := len(tail.String()); got > maxServiceLogTailBytes {
		t.Fatalf("tail length = %d, want at most %d", got, maxServiceLogTailBytes)
	}
}

func TestSanitizedLogTailRetainsCrashSummaryWithLatestDiagnostics(t *testing.T) {
	tail := &sanitizedLogTail{}
	tail.add("panic: token=<redacted>")
	for i := 0; i < 20; i++ {
		tail.add(strings.Repeat("stack", 128))
	}
	got := tail.String()
	if len(got) > maxServiceLogTailBytes {
		t.Fatalf("tail length = %d, want at most %d", len(got), maxServiceLogTailBytes)
	}
	if !strings.Contains(got, "panic: token=<redacted>") || !strings.Contains(got, "stack") {
		t.Fatalf("tail lost crash summary or diagnostics: %q", got)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) {
	return 0, io.ErrUnexpectedEOF
}

func TestCopyServiceOutputSanitizesReadErrors(t *testing.T) {
	logger := &recordingServiceLogger{}
	tail := &sanitizedLogTail{}
	copyServiceOutputWithSecrets(failingReader{}, logger, tail, "configured-secret")
	if len(logger.errors) != 1 || !strings.Contains(tail.String(), "capture failed") {
		t.Fatalf("capture error logger=%v tail=%q", logger.errors, tail.String())
	}
}

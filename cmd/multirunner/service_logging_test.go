package main

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/GerardSmit/multirunner/internal/config"
)

func TestServiceLogWriterRoutesLevels(t *testing.T) {
	logger := &recordingServiceLogger{}
	writer := &serviceLogWriter{logger: logger}
	for _, message := range []string{
		"time=x level=INFO msg=ready\n",
		"time=x level=WARN msg=slow\n",
		"time=x level=ERROR msg=failed\n",
	} {
		if _, err := writer.Write([]byte(message)); err != nil {
			t.Fatal(err)
		}
	}
	if len(logger.infos) != 1 || len(logger.warnings) != 1 || len(logger.errors) != 1 {
		t.Fatalf("routed info=%v warn=%v error=%v", logger.infos, logger.warnings, logger.errors)
	}
}

func TestServiceLogWriterRedactsRuntimeOutput(t *testing.T) {
	logger := &recordingServiceLogger{}
	writer := &serviceLogWriter{logger: logger}
	input := "authorization=Bearer-secret token=github_pat_abcdefghijklmnopqrstuvwxyz JIT_CONFIG=BASE64-JIT-BLOB"
	if _, err := writer.Write([]byte(input)); err != nil {
		t.Fatal(err)
	}
	logged := strings.Join(logger.infos, "\n")
	for _, secret := range []string{"Bearer-secret", "github_pat_abcdefghijklmnopqrstuvwxyz", "BASE64-JIT-BLOB"} {
		if strings.Contains(logged, secret) {
			t.Errorf("runtime log leaked %q: %q", secret, logged)
		}
	}
}

func TestServiceLogWriterRedactsGeneratedCacheTokenPath(t *testing.T) {
	logger := &recordingServiceLogger{}
	writer := &serviceLogWriter{logger: logger}
	input := "launch failed: ACTIONS_RESULTS_URL=http://cache:3000/_mr/generated-secret/results"
	if _, err := writer.Write([]byte(input)); err != nil {
		t.Fatal(err)
	}
	logged := strings.Join(logger.infos, "\n")
	if strings.Contains(logged, "generated-secret") {
		t.Fatalf("runtime log leaked generated cache token: %q", logged)
	}
	if !strings.Contains(logged, "/_mr/<redacted>/results") {
		t.Fatalf("runtime log lost cache URL context: %q", logged)
	}
}

func TestServiceLogWriterRedactsBareEncodedJITValue(t *testing.T) {
	logger := &recordingServiceLogger{}
	writer := &serviceLogWriter{logger: logger}
	jit := strings.Repeat("AbCdEf0123456789", 16)
	if _, err := writer.Write([]byte("job output: " + jit)); err != nil {
		t.Fatal(err)
	}
	logged := strings.Join(logger.infos, "\n")
	if strings.Contains(logged, jit) || !strings.Contains(logged, "<redacted-encoded-secret>") {
		t.Fatalf("runtime log did not redact bare encoded JIT config: %q", logged)
	}
}

func TestCopyServiceOutputRoutesLinesAndRedactsJSON(t *testing.T) {
	logger := &recordingServiceLogger{}
	copyServiceOutput(strings.NewReader("ready\n{\"jit_config\":\"secret-jit\"}\n"), logger)
	if len(logger.infos) != 2 {
		t.Fatalf("captured lines = %v, want 2", logger.infos)
	}
	logged := strings.Join(logger.infos, "\n")
	if strings.Contains(logged, "secret-jit") {
		t.Fatalf("captured output leaked JIT config: %q", logged)
	}
}

func TestCopyServiceOutputRedactsMultilinePrivateKey(t *testing.T) {
	logger := &recordingServiceLogger{}
	input := "before\n-----BEGIN PRIVATE KEY-----\nPRIVATE-BODY\n-----END PRIVATE KEY-----\nafter\n"
	copyServiceOutput(strings.NewReader(input), logger)
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

func TestCaptureServiceOutputLeavesInteractiveStreamsAttached(t *testing.T) {
	restore, err := captureServiceOutput(true, &recordingServiceLogger{})
	if err != nil {
		t.Fatal(err)
	}
	restore()
}

func TestCaptureServiceOutputRoutesWindowsRuntimeStreams(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows redirects runtime streams into the native service logger")
	}
	logger := &recordingServiceLogger{}
	restore, err := captureServiceOutput(false, logger)
	if err != nil {
		t.Fatal(err)
	}
	_, stdoutErr := fmt.Fprintln(os.Stdout, "runtime stdout")
	_, stderrErr := fmt.Fprintln(os.Stderr, "runtime stderr")
	restore()
	if stdoutErr != nil || stderrErr != nil {
		t.Fatalf("write runtime streams: stdout=%v stderr=%v", stdoutErr, stderrErr)
	}
	logged := strings.Join(logger.infos, "\n")
	for _, want := range []string{"runtime stdout", "runtime stderr"} {
		if !strings.Contains(logged, want) {
			t.Errorf("captured output missing %q: %q", want, logged)
		}
	}
}

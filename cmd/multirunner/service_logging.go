package main

import (
	"bufio"
	"io"
	"log/slog"
	"regexp"
	"sort"
	"strings"

	"github.com/kardianos/service"

	"github.com/GerardSmit/multirunner/internal/config"
)

var (
	sensitiveJSONValue  = regexp.MustCompile(`(?i)("(?:authorization|credential|jit[_-]?config|password|secret|token)"\s*:\s*)"[^"]*"`)
	sensitiveAssignment = regexp.MustCompile(`(?i)\b(authorization|credential|jit[_-]?config|password|secret|token)(\s*=\s*)(?:"[^"]*"|'[^']*'|[^\s,;]+)`)
	githubToken         = regexp.MustCompile(`\b(gh[pousr]_[A-Za-z0-9_]{20,}|github_pat_[A-Za-z0-9_]{20,})\b`)
	privateKey          = regexp.MustCompile(`(?s)-----BEGIN [^-]*PRIVATE KEY-----.*?-----END [^-]*PRIVATE KEY-----`)
	cacheTokenPath      = regexp.MustCompile(`/_mr/[^/?\s]+`)
	bareEncodedSecret   = regexp.MustCompile(`\b[A-Za-z0-9+/]{128,}={0,2}\b`)
)

type serviceLogWriter struct {
	logger service.Logger
}

func (w *serviceLogWriter) Write(data []byte) (int, error) {
	if w == nil || w.logger == nil {
		return len(data), nil
	}
	message := sanitizeLogText(strings.TrimSpace(string(data)))
	var err error
	switch {
	case strings.Contains(message, "level=ERROR"), strings.Contains(message, `"level":"ERROR"`):
		err = w.logger.Error(message)
	case strings.Contains(message, "level=WARN"), strings.Contains(message, `"level":"WARN"`):
		err = w.logger.Warning(message)
	default:
		err = w.logger.Info(message)
	}
	return len(data), err
}

type redactingWriter struct {
	writer  io.Writer
	secrets []string
}

func newRedactingWriter(writer io.Writer, secrets ...string) io.Writer {
	if writer == nil {
		return io.Discard
	}
	filtered := secrets[:0]
	for _, secret := range secrets {
		if secret != "" {
			filtered = append(filtered, secret)
		}
	}
	sort.Slice(filtered, func(i, j int) bool { return len(filtered[i]) > len(filtered[j]) })
	return &redactingWriter{writer: writer, secrets: filtered}
}

func (w *redactingWriter) Write(data []byte) (int, error) {
	message := sanitizeLogTextWithSecrets(string(data), w.secrets...)
	if _, err := io.WriteString(w.writer, message); err != nil {
		return 0, err
	}
	return len(data), nil
}

func sanitizeLogTextWithSecrets(message string, secrets ...string) string {
	for _, secret := range secrets {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "<redacted>")
		}
	}
	return sanitizeLogText(message)
}

func sanitizeLogText(message string) string {
	message = privateKey.ReplaceAllString(message, "<redacted-private-key>")
	message = githubToken.ReplaceAllString(message, "<redacted-token>")
	message = cacheTokenPath.ReplaceAllString(message, "/_mr/<redacted>")
	message = bareEncodedSecret.ReplaceAllString(message, "<redacted-encoded-secret>")
	message = sensitiveJSONValue.ReplaceAllString(message, `${1}"<redacted>"`)
	return sensitiveAssignment.ReplaceAllString(message, "$1$2<redacted>")
}

func copyServiceOutput(reader io.Reader, logger service.Logger) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	inPrivateKey := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "-----BEGIN") && strings.Contains(line, "PRIVATE KEY-----") {
			inPrivateKey = true
			_ = logger.Info("<redacted-private-key>")
			continue
		}
		if inPrivateKey {
			if strings.Contains(line, "-----END") && strings.Contains(line, "PRIVATE KEY-----") {
				inPrivateKey = false
			}
			continue
		}
		_ = logger.Info(sanitizeLogText(line))
	}
	if err := scanner.Err(); err != nil {
		_ = logger.Error("service output capture failed: " + sanitizeLogText(err.Error()))
	}
}

func newLogger(logConfig config.Log, output io.Writer, secrets ...string) *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(logConfig.Level) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: level}
	writer := newRedactingWriter(output, secrets...)
	if strings.ToLower(logConfig.Format) == "json" {
		return slog.New(slog.NewJSONHandler(writer, opts))
	}
	return slog.New(slog.NewTextHandler(writer, opts))
}

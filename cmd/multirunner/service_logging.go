package main

import (
	"bufio"
	"errors"
	"io"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/kardianos/service"

	"github.com/GerardSmit/multirunner/internal/config"
)

const (
	maxServiceLogRecordBytes = 8 * 1024
	maxServiceLogTailBytes   = 2 * 1024
	serviceLogReadBuffer     = 4 * 1024
)

var (
	sensitiveJSONValue  = regexp.MustCompile(`(?i)("(?:authorization|credential|jit[_-]?config|password|secret|token)"\s*:\s*)"[^"]*"`)
	sensitiveAssignment = regexp.MustCompile(`(?i)\b(authorization|credential|jit[_-]?config|password|secret|token)(\s*=\s*)(?:"[^"]*"|'[^']*'|[^\s,;]+)`)
	githubToken         = regexp.MustCompile(`\b(gh[pousr]_[A-Za-z0-9_]{20,}|github_pat_[A-Za-z0-9_]{20,})\b`)
	privateKey          = regexp.MustCompile(`(?s)-----BEGIN [^-]*PRIVATE KEY-----.*?-----END [^-]*PRIVATE KEY-----`)
	cacheTokenPath      = regexp.MustCompile(`/_mr/[^/?\s]+`)
	bareEncodedSecret   = regexp.MustCompile(`\b[A-Za-z0-9+/]{128,}={0,2}\b`)
)

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
	message = strings.Map(func(value rune) rune {
		switch {
		case value == '\n', value == '\t':
			return value
		case value < 0x20, value == 0x7f, unicode.Is(unicode.Cf, value):
			return -1
		default:
			return value
		}
	}, message)
	message = privateKey.ReplaceAllString(message, "<redacted-private-key>")
	message = githubToken.ReplaceAllString(message, "<redacted-token>")
	message = cacheTokenPath.ReplaceAllString(message, "/_mr/<redacted>")
	message = bareEncodedSecret.ReplaceAllString(message, "<redacted-encoded-secret>")
	message = sensitiveJSONValue.ReplaceAllString(message, `${1}"<redacted>"`)
	return sensitiveAssignment.ReplaceAllString(message, "$1$2<redacted>")
}

type sanitizedLogTail struct {
	mu      sync.Mutex
	summary string
	content string
}

func (t *sanitizedLogTail) add(message string) {
	if t == nil || message == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.summary == "" && (strings.HasPrefix(message, "panic:") || strings.HasPrefix(message, "fatal error:")) {
		t.summary = truncateLogSummary(message, maxServiceLogTailBytes/4)
	}
	if t.content != "" {
		t.content += "\n"
	}
	t.content += message
	contentBudget := maxServiceLogTailBytes
	if t.summary != "" {
		contentBudget -= len(t.summary) + 1
	}
	t.content = truncateLogTailValue(t.content, contentBudget)
}

func (t *sanitizedLogTail) String() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.summary != "" && !strings.Contains(t.content, t.summary) {
		return t.summary + "\n" + t.content
	}
	return t.content
}

func truncateLogTailValue(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	value = value[len(value)-maxBytes:]
	for !utf8.ValidString(value) {
		value = value[1:]
	}
	return value
}

func truncateLogSummary(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	value = value[:maxBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func copyServiceOutputWithSecrets(reader io.Reader, logger service.Logger, tail *sanitizedLogTail, secrets ...string) {
	buffered := bufio.NewReaderSize(reader, serviceLogReadBuffer)
	line := make([]byte, 0, serviceLogReadBuffer)
	truncated := false
	inPrivateKey := false
	beginMarker := false
	endMarker := false
	markerTail := ""

	for {
		fragment, err := buffered.ReadSlice('\n')
		if len(fragment) > 0 {
			markerProbe := markerTail + string(fragment)
			beginMarker = beginMarker || containsPrivateKeyMarker(markerProbe, "BEGIN")
			endMarker = endMarker || containsPrivateKeyMarker(markerProbe, "END")
			if len(markerProbe) > 64 {
				markerTail = markerProbe[len(markerProbe)-64:]
			} else {
				markerTail = markerProbe
			}

			if !truncated {
				remaining := maxServiceLogRecordBytes - len(line)
				if len(fragment) > remaining {
					line = append(line, fragment[:remaining]...)
					truncated = true
				} else {
					line = append(line, fragment...)
				}
			}
		}

		lineComplete := err == nil || errors.Is(err, io.EOF)
		if lineComplete && (len(line) > 0 || truncated) {
			message := strings.TrimRight(string(line), "\r\n")
			if truncated {
				message += " <truncated>"
			}
			emitServiceOutputLine(message, beginMarker, endMarker, &inPrivateKey, logger, tail, secrets...)
			line = line[:0]
			truncated = false
			beginMarker = false
			endMarker = false
			markerTail = ""
		}

		switch {
		case err == nil, errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			return
		default:
			message := sanitizeLogTextWithSecrets("service output capture failed: "+err.Error(), secrets...)
			if logger != nil {
				_ = logger.Error(message)
			}
			tail.add(message)
			return
		}
	}
}

func emitServiceOutputLine(message string, beginMarker, endMarker bool, inPrivateKey *bool, logger service.Logger, tail *sanitizedLogTail, secrets ...string) {
	if beginMarker {
		*inPrivateKey = !endMarker
		message = "<redacted-private-key>"
	} else if *inPrivateKey {
		if endMarker {
			*inPrivateKey = false
		}
		return
	} else {
		message = sanitizeLogTextWithSecrets(message, secrets...)
	}
	writeServiceLog(logger, message)
	tail.add(message)
}

func writeServiceLog(logger service.Logger, message string) {
	if logger == nil {
		return
	}
	switch {
	case strings.Contains(message, "level=ERROR"), strings.Contains(message, `"level":"ERROR"`):
		_ = logger.Error(message)
	case strings.Contains(message, "level=WARN"), strings.Contains(message, `"level":"WARN"`):
		_ = logger.Warning(message)
	default:
		_ = logger.Info(message)
	}
}

func containsPrivateKeyMarker(message, boundary string) bool {
	return strings.Contains(message, "-----"+boundary) && strings.Contains(message, "PRIVATE KEY-----")
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

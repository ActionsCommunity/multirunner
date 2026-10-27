// Package secretmask applies defense-in-depth masking to transient operator data.
package secretmask

import (
	"regexp"
	"strings"
)

var patterns = []*regexp.Regexp{
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`),
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=-]{12,}`),
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`),
}

func Text(value string, secrets []string) string {
	for _, secret := range secrets {
		secret = strings.TrimSpace(secret)
		if len(secret) >= 4 {
			value = strings.ReplaceAll(value, secret, "***")
		}
	}
	for _, pattern := range patterns {
		value = pattern.ReplaceAllString(value, "***")
	}
	return value
}

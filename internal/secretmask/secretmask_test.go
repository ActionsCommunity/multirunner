package secretmask

import (
	"strings"
	"testing"
)

func TestTextMasksSecretsAndBuiltInPatterns(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		secrets   []string
		want      string
		mustNotIn []string
	}{
		{
			name:      "explicit secret >= 4 chars trimmed",
			input:     "token=my-secret-value-123 and keep short=abc",
			secrets:   []string{"  my-secret-value-123  ", "abc"},
			want:      "token=*** and keep short=abc",
			mustNotIn: []string{"my-secret-value-123"},
		},
		{
			name:      "github classic and fine-grained tokens",
			input:     "ghp_1234567890abcdefghij1234 and github_pat_1234567890_abcdefghij1234",
			want:      "*** and ***",
			mustNotIn: []string{"ghp_1234567890", "github_pat_1234567890"},
		},
		{
			name:      "bearer token aws key and jwt",
			input:     "Authorization: Bearer abcdef123456._~+/=- key=AKIAIOSFODNN7EXAMPLE jwt=eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.c2lnbmF0dXJlMTIzNDU",
			want:      "Authorization: *** key=*** jwt=***",
			mustNotIn: []string{"abcdef123456", "AKIAIOSFODNN7EXAMPLE", "eyJhbGciOiJIUzI1NiJ9"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Text(tc.input, tc.secrets)
			if got != tc.want {
				t.Fatalf("Text() = %q, want %q", got, tc.want)
			}
			for _, secret := range tc.mustNotIn {
				if strings.Contains(got, secret) {
					t.Fatalf("Text() leaked %q in %q", secret, got)
				}
			}
		})
	}
}

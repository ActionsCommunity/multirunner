package main

import (
	"reflect"
	"testing"

	"github.com/GerardSmit/multirunner/internal/config"
)

func TestConfiguredServiceTokenPathsSelectsOnlyActiveExplicitOAuthSidecar(t *testing.T) {
	const path = `C:\separate-auth\github-user-token.json`
	for _, test := range []struct {
		name string
		cfg  *config.Config
		want []string
	}{
		{"nil config", nil, nil},
		{"no configured token", &config.Config{}, nil},
		{"explicit OAuth token", &config.Config{Auth: config.Auth{TokenPath: path}}, []string{path}},
		{"PAT overrides inactive token path", &config.Config{Auth: config.Auth{PAT: "test-pat", TokenPath: path}}, nil},
		{"installation app overrides inactive token path", &config.Config{Auth: config.Auth{AppID: 1, TokenPath: path}}, nil},
		{"relative path retained without profile inference", &config.Config{Auth: config.Auth{TokenPath: "token.json"}}, []string{"token.json"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := configuredServiceTokenPaths(test.cfg); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("configured token paths = %q, want %q", got, test.want)
			}
		})
	}
}

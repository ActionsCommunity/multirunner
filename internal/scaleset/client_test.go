package scaleset

import "testing"

func TestTargetURLPerScope(t *testing.T) {
	cases := []struct {
		name              string
		base, scope       string
		owner, repo, want string
	}{
		{"repo", "https://github.com", "repo", "octocat", "hello-world", "https://github.com/octocat/hello-world"},
		{"org", "https://github.com", "org", "octocat", "", "https://github.com/octocat"},
		{"enterprise", "https://github.com", "enterprise", "acme", "", "https://github.com/enterprises/acme"},
		{"trailing slash is trimmed", "https://github.com/", "org", "octocat", "", "https://github.com/octocat"},
		{"empty base defaults to dotcom", "", "org", "octocat", "", "https://github.com/octocat"},
		{"ghes host is preserved", "https://ghe.example.com", "repo", "o", "r", "https://ghe.example.com/o/r"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := TargetURL(tc.base, tc.scope, tc.owner, tc.repo)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTargetURLRejectsIncompleteTargets(t *testing.T) {
	cases := []struct {
		name              string
		scope, owner, rep string
	}{
		{"repo without repo name", "repo", "octocat", ""},
		{"repo without owner", "repo", "", "hello-world"},
		{"org without owner", "org", "", ""},
		{"enterprise without owner", "enterprise", "", ""},
		// A scale set attaches to one target, so a fan-out scope has nothing to
		// bind to and must fail rather than silently pick one.
		{"unsupported scope", "repos", "octocat", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := TargetURL("https://github.com", tc.scope, tc.owner, tc.rep); err == nil {
				t.Fatal("expected an error, got nil")
			}
		})
	}
}

func TestNewClientRequiresCredentials(t *testing.T) {
	if _, err := NewClient(ClientOptions{TargetURL: "https://github.com/o/r"}); err == nil {
		t.Fatal("expected an error when neither a PAT nor App credentials are set")
	}
}

func TestNewClientAppRequiresKeyPath(t *testing.T) {
	_, err := NewClient(ClientOptions{
		TargetURL:      "https://github.com/o/r",
		AppID:          123,
		InstallationID: 456,
	})
	if err == nil {
		t.Fatal("expected an error when the App private key path is missing")
	}
}

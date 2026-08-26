package scaleset

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/actions/scaleset"
)

// TargetURL builds the GitHub registration URL a scale set attaches to.
//
// A scale set belongs to exactly one target, which is why scaleset mode needs a
// single repo, org or enterprise rather than a fan-out across many.
func TargetURL(baseURL, scope, owner, repo string) (string, error) {
	base := strings.TrimSuffix(baseURL, "/")
	if base == "" {
		base = "https://github.com"
	}
	switch scope {
	case "repo":
		if owner == "" || repo == "" {
			return "", fmt.Errorf("scope=repo needs github.owner and github.repo")
		}
		return fmt.Sprintf("%s/%s/%s", base, owner, repo), nil
	case "org":
		if owner == "" {
			return "", fmt.Errorf("scope=org needs github.owner")
		}
		return fmt.Sprintf("%s/%s", base, owner), nil
	case "enterprise":
		if owner == "" {
			return "", fmt.Errorf("scope=enterprise needs github.owner")
		}
		return fmt.Sprintf("%s/enterprises/%s", base, owner), nil
	default:
		return "", fmt.Errorf("provisioning: scaleset supports scope repo|org|enterprise, got %q", scope)
	}
}

// ClientOptions carries the credentials a scale set client needs, in the shape
// multirunner's config holds them.
type ClientOptions struct {
	TargetURL string
	PAT       string
	// AppID, InstallationID and PrivateKeyPath describe GitHub App auth. The
	// scale set client wants the key as PEM content rather than a path, so the
	// file is read here.
	AppID          int64
	InstallationID int64
	PrivateKeyPath string
}

// NewClient builds a scale set client, preferring a GitHub App when one is
// configured because org-level scale sets generally require it.
func NewClient(opts ClientOptions) (*scaleset.Client, error) {
	if opts.PAT == "" && opts.AppID == 0 {
		return nil, fmt.Errorf("provisioning: scaleset needs auth.pat or GitHub App credentials")
	}
	if opts.PAT == "" {
		if opts.PrivateKeyPath == "" {
			return nil, fmt.Errorf("provisioning: scaleset with a GitHub App needs auth.private_key_path")
		}
		pem, err := os.ReadFile(opts.PrivateKeyPath)
		if err != nil {
			return nil, fmt.Errorf("read GitHub App private key: %w", err)
		}
		return scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{
			GitHubConfigURL: opts.TargetURL,
			GitHubAppAuth: scaleset.GitHubAppAuth{
				ClientID:       strconv.FormatInt(opts.AppID, 10),
				InstallationID: opts.InstallationID,
				PrivateKey:     string(pem),
			},
			SystemInfo: systemInfo(0),
		})
	}
	return scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
		GitHubConfigURL:     opts.TargetURL,
		PersonalAccessToken: opts.PAT,
		SystemInfo:          systemInfo(0),
	})
}

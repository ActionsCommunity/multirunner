package scaleset

import (
	"net/http"
	"net/url"

	"github.com/actions/scaleset"
	"github.com/hashicorp/go-retryablehttp"

	"github.com/GerardSmit/multirunner/internal/ghapp"
)

// newDeviceClient builds a scale set client authenticated with a GitHub App
// device-flow user access token.
//
// The scale set library captures the token at construction and re-sends it (as a
// Bearer header) whenever it refreshes its Actions admin session, which can be
// hours into a long-lived session — by then a user token has expired. A
// refreshing transport intercepts exactly that header and swaps in a freshly
// refreshed token, leaving every other request (notably the RemoteAuth
// admin-token calls that drive the long-poll and job acquisition) untouched.
func newDeviceClient(opts ClientOptions) (*scaleset.Client, error) {
	refresher, err := ghapp.NewTokenRefresher(opts.ClientID, baseURLFromTarget(opts.TargetURL), opts.TokenPath)
	if err != nil {
		return nil, err
	}
	// The library re-sends this exact token value, so the hook matches on it
	// rather than on the rotated token it will later hold.
	initial := refresher.Current()
	sentinel := "Bearer " + initial

	// The rewrite runs in RequestLogHook, not in a wrapping RoundTripper: the
	// library re-derives its retryable client mid-session (client.go:869) and
	// type-asserts HTTPClient.Transport to *http.Transport, so replacing the
	// transport makes the admin-connection call fail outright. The hook sees the
	// same request and leaves the transport's concrete type alone. It cannot
	// return an error, so a failed refresh leaves the stale header and surfaces
	// as the API's own 401.
	rc := newDeviceRetryableClient(refresher, sentinel)

	return scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
		GitHubConfigURL:     opts.TargetURL,
		PersonalAccessToken: initial,
		SystemInfo:          systemInfo(0),
	}, scaleset.WithRetryableHTTPClint(rc))
}

// baseURLFromTarget recovers the GitHub web host (scheme + host) from a scale set
// target URL so the device/oauth endpoints resolve against the same host, which
// matters for GHES. An empty or unparseable value falls back to the ghapp default.
func baseURLFromTarget(target string) string {
	u, err := url.Parse(target)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// newDeviceRetryableClient builds the retryable client the library expects,
// rewriting the pinned user token to a refreshed one before each attempt. The
// transport is left as the default *http.Transport on purpose; see the comment
// in newDeviceClient.
func newDeviceRetryableClient(refresher *ghapp.TokenRefresher, sentinel string) *retryablehttp.Client {
	rc := retryablehttp.NewClient()
	rc.RequestLogHook = func(_ retryablehttp.Logger, req *http.Request, _ int) {
		if req.Header.Get("Authorization") != sentinel {
			return
		}
		access, err := refresher.AccessToken(req.Context())
		if err != nil {
			return
		}
		req.Header.Set("Authorization", "Bearer "+access)
	}
	return rc
}

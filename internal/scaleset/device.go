package scaleset

import (
	"net/http"
	"net/url"
	"time"

	"github.com/actions/scaleset"

	"github.com/GerardSmit/multirunner/internal/ghapp"
)

// newDeviceClient builds a scale set client authenticated with a GitHub App
// device-flow user access token.
//
// The scale set library captures the token at construction and re-sends it (as a
// Bearer header) whenever it refreshes its Actions admin session, which can be
// hours into a long-lived session — by then a user token has expired. A
// refreshing HTTPClient intercepts exactly that header and swaps in a freshly
// refreshed token, leaving every other request (notably the RemoteAuth
// admin-token calls that drive the long-poll and job acquisition) untouched.
func newDeviceClient(opts ClientOptions) (*scaleset.Client, error) {
	refresher, err := ghapp.NewTokenRefresher(opts.ClientID, baseURLFromTarget(opts.TargetURL), opts.TokenPath)
	if err != nil {
		return nil, err
	}
	// The library re-sends this exact token value, so the client matches on it
	// rather than on the rotated token it will later hold.
	initial := refresher.Current()
	sentinel := "Bearer " + initial

	hc := newDeviceHTTPClient(refresher, sentinel)

	return scaleset.NewClientWithPersonalAccessToken(
		scaleset.NewClientWithPersonalAccessTokenConfig{
			GitHubConfigURL:     opts.TargetURL,
			PersonalAccessToken: initial,
			SystemInfo:          systemInfo(0),
		},
		scaleset.WithHTTPClient(hc),
		scaleset.WithRetry(scaleset.RetryConfig{
			Max:     3,
			WaitMax: 5 * time.Second,
		}),
	)
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

type deviceHTTPClient struct {
	base      *http.Client
	refresher *ghapp.TokenRefresher
	sentinel  string
}

// newDeviceHTTPClient builds the HTTP client passed to WithHTTPClient,
// rewriting the pinned user token to a refreshed one before each attempt while
// keeping the SDK's default *http.Transport. If refreshing fails, the stale
// header is left in place so the API's own 401 surfaces to the caller.
func newDeviceHTTPClient(refresher *ghapp.TokenRefresher, sentinel string) *deviceHTTPClient {
	return &deviceHTTPClient{
		base:      scaleset.NewHTTPClient(scaleset.HTTPClientConfig{}),
		refresher: refresher,
		sentinel:  sentinel,
	}
}

func (c *deviceHTTPClient) Do(req *http.Request) (*http.Response, error) {
	if req.Header.Get("Authorization") == c.sentinel {
		if access, err := c.refresher.AccessToken(req.Context()); err == nil {
			req = req.Clone(req.Context())
			req.Header.Set("Authorization", "Bearer "+access)
		}
	}
	return c.base.Do(req)
}

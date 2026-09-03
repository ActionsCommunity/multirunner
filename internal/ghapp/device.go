package ghapp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultClientID is the client id of the shared multirunner GitHub App used by
// the device-flow connect path for organization targets. A GitHub App device flow
// uses no client secret, so this identifier is not sensitive; config may override
// it via auth.client_id.
//
// It carries organization_self_hosted_runners:write, which covers org runners and
// nothing else. Repository runners need repository administration:write, a
// permission an org-scoped App must not hold, so that target uses a second App
// (DefaultPersonalClientID) rather than widening this one.
const DefaultClientID = "Iv23liZGKUct4sAKjq2m"

// DefaultPersonalClientID is the client id of the shared App used for repository
// targets. It carries administration:write (plus metadata:read), which is what
// registering a repository runner requires.
const DefaultPersonalClientID = "Iv23liZVB5CPke0jCFQi"

// DefaultBaseURL is the GitHub.com web host that serves the device and token
// endpoints; GHES callers pass their own base.
const DefaultBaseURL = "https://github.com"

// DefaultAPIBase is the GitHub.com REST host used to list a user's App
// installations; GHES callers pass <base>/api/v3.
const DefaultAPIBase = "https://api.github.com"

// DefaultAppSlug is the shared App's slug (display name "Multirunner Connect"),
// used to build an install URL when a user has not yet installed the App
// anywhere (so no installation reports one). Kept next to DefaultClientID
// because it can change with the App's name.
const DefaultAppSlug = "multirunner-connect"

// DefaultPersonalAppSlug is the repository App's slug, used the same way.
const DefaultPersonalAppSlug = "multirunner-connect-personal"

// pollWait is time.After, indirected so tests can remove the real between-poll
// delay without changing the polling logic.
var pollWait = time.After

// DeviceCode is the device/user code pair returned by the device authorization
// request.
type DeviceCode struct {
	DeviceCode      string
	UserCode        string
	VerificationURI string
	ExpiresIn       int
	Interval        int
}

// UserToken is a GitHub App user access token plus its refresh token. Expiry and
// RefreshExpiry are zero when the App does not expire user tokens.
type UserToken struct {
	AccessToken   string
	RefreshToken  string
	Expiry        time.Time
	RefreshExpiry time.Time
}

// Installation is one App installation visible to a user access token.
type Installation struct {
	ID      int64
	Account string // login of the org/user the App is installed on
	// IsOrg distinguishes an organization installation from a personal one.
	// organization_self_hosted_runners only applies to orgs, so a personal
	// installation can never serve org runner management.
	IsOrg   bool
	AppSlug string
}

// tokenResponse is the shared shape of the device-code, token-exchange, and
// refresh responses; GitHub returns either the token fields or an error field.
type tokenResponse struct {
	AccessToken           string `json:"access_token"`
	RefreshToken          string `json:"refresh_token"`
	ExpiresIn             int    `json:"expires_in"`
	RefreshTokenExpiresIn int    `json:"refresh_token_expires_in"`
	Error                 string `json:"error"`
	ErrorDescription      string `json:"error_description"`
	Interval              int    `json:"interval"`
}

// RequestDeviceCode starts the device authorization flow and returns the code
// pair the caller must present to the user.
func RequestDeviceCode(ctx context.Context, clientID, baseURL string) (*DeviceCode, error) {
	clientID = orDefault(clientID, DefaultClientID)
	baseURL = strings.TrimRight(orDefault(baseURL, DefaultBaseURL), "/")

	var out struct {
		DeviceCode       string `json:"device_code"`
		UserCode         string `json:"user_code"`
		VerificationURI  string `json:"verification_uri"`
		ExpiresIn        int    `json:"expires_in"`
		Interval         int    `json:"interval"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := postForm(ctx, baseURL+"/login/device/code", url.Values{"client_id": {clientID}}, &out); err != nil {
		return nil, err
	}
	if out.Error != "" {
		return nil, deviceFlowError(out.Error, out.ErrorDescription)
	}
	if out.DeviceCode == "" || out.UserCode == "" {
		return nil, fmt.Errorf("device code request returned no code")
	}
	return &DeviceCode{
		DeviceCode:      out.DeviceCode,
		UserCode:        out.UserCode,
		VerificationURI: out.VerificationURI,
		ExpiresIn:       out.ExpiresIn,
		Interval:        out.Interval,
	}, nil
}

// PollDeviceToken polls the token endpoint at the code's interval until the user
// authorizes, the code expires, or ctx is cancelled. slow_down responses widen
// the interval; expired_token and access_denied stop with an actionable error.
func PollDeviceToken(ctx context.Context, clientID, baseURL string, dc *DeviceCode) (*UserToken, error) {
	clientID = orDefault(clientID, DefaultClientID)
	baseURL = strings.TrimRight(orDefault(baseURL, DefaultBaseURL), "/")

	interval := dc.Interval
	if interval < 1 {
		interval = 5
	}
	lifetime := time.Duration(dc.ExpiresIn) * time.Second
	if lifetime <= 0 {
		lifetime = 15 * time.Minute
	}
	deadline := time.Now().Add(lifetime)

	form := url.Values{
		"client_id":   {clientID},
		"device_code": {dc.DeviceCode},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
	}

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-pollWait(time.Duration(interval) * time.Second):
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("device authorization timed out after %s; re-run `multirunner connect`", lifetime)
		}

		var out tokenResponse
		if err := postForm(ctx, baseURL+"/login/oauth/access_token", form, &out); err != nil {
			return nil, err
		}
		switch out.Error {
		case "":
			if out.AccessToken == "" {
				return nil, fmt.Errorf("device token exchange returned no access token")
			}
			return tokenFromResponse(out, time.Now()), nil
		case "authorization_pending":
			// Not authorized yet; keep polling at the current interval.
		case "slow_down":
			// GitHub asks us to back off; adopt the wider interval it returns,
			// or add the spec's 5s when it does not.
			if out.Interval > interval {
				interval = out.Interval
			} else {
				interval += 5
			}
		case "expired_token":
			return nil, fmt.Errorf("device code expired before you authorized it; re-run `multirunner connect`")
		case "access_denied":
			return nil, fmt.Errorf("authorization was denied")
		default:
			return nil, deviceFlowError(out.Error, out.ErrorDescription)
		}
	}
}

// RefreshUserToken exchanges a refresh token for a new user access token. A
// GitHub error is turned into an actionable message rather than a silent stall,
// because the transport calls this on the request path.
func RefreshUserToken(ctx context.Context, clientID, baseURL, refreshToken string) (*UserToken, error) {
	clientID = orDefault(clientID, DefaultClientID)
	baseURL = strings.TrimRight(orDefault(baseURL, DefaultBaseURL), "/")

	var out tokenResponse
	if err := postForm(ctx, baseURL+"/login/oauth/access_token", url.Values{
		"client_id":     {clientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	}, &out); err != nil {
		return nil, fmt.Errorf("refreshing the GitHub user token failed (%w); re-run `multirunner connect` to re-authorize", err)
	}
	if out.Error != "" {
		return nil, fmt.Errorf("refreshing the GitHub user token was rejected (%s); re-run `multirunner connect` to re-authorize", describeError(out.Error, out.ErrorDescription))
	}
	if out.AccessToken == "" {
		return nil, fmt.Errorf("token refresh returned no access token; re-run `multirunner connect` to re-authorize")
	}
	return tokenFromResponse(out, time.Now()), nil
}

// UserInstallations lists the App installations the user access token can see,
// so connect can confirm the App is installed on the target account.
func UserInstallations(ctx context.Context, apiBase, accessToken string) ([]Installation, error) {
	apiBase = strings.TrimRight(orDefault(apiBase, DefaultAPIBase), "/")

	var all []Installation
	for page := 1; ; page++ {
		endpoint := fmt.Sprintf("%s/user/installations?per_page=100&page=%d", apiBase, page)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("Authorization", "Bearer "+accessToken)
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

		resp, err := oauthClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("list user installations: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("list user installations: status %d", resp.StatusCode)
		}
		var body struct {
			Installations []struct {
				ID      int64 `json:"id"`
				Account struct {
					Login string `json:"login"`
					Type  string `json:"type"`
				} `json:"account"`
				AppSlug string `json:"app_slug"`
			} `json:"installations"`
		}
		err = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("decode user installations: %w", err)
		}
		for _, in := range body.Installations {
			all = append(all, Installation{
				ID:      in.ID,
				Account: in.Account.Login,
				IsOrg:   strings.EqualFold(in.Account.Type, "Organization"),
				AppSlug: in.AppSlug,
			})
		}
		if len(body.Installations) < 100 {
			return all, nil
		}
	}
}

// MatchInstallation returns the installation on account (case-insensitive), if
// any, alongside a best-effort App slug for building an install URL.
func MatchInstallation(installs []Installation, account string) (inst Installation, slug string, ok bool) {
	for _, in := range installs {
		if in.AppSlug != "" {
			slug = in.AppSlug
		}
		if strings.EqualFold(in.Account, account) {
			return in, orDefault(in.AppSlug, slug), true
		}
	}
	return Installation{}, orDefault(slug, DefaultAppSlug), false
}

func tokenFromResponse(r tokenResponse, now time.Time) *UserToken {
	t := &UserToken{AccessToken: r.AccessToken, RefreshToken: r.RefreshToken}
	if r.ExpiresIn > 0 {
		t.Expiry = now.Add(time.Duration(r.ExpiresIn) * time.Second)
	}
	if r.RefreshTokenExpiresIn > 0 {
		t.RefreshExpiry = now.Add(time.Duration(r.RefreshTokenExpiresIn) * time.Second)
	}
	return t
}

// postForm POSTs form as application/x-www-form-urlencoded and decodes the JSON
// response into out. Only the endpoint (never the form body, which carries the
// device code or refresh token) appears in error messages.
func postForm(ctx context.Context, endpoint string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := oauthClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	// The OAuth endpoints answer 200 even for errors (the body carries `error`),
	// so anything else is a transport-level problem — a proxy, a wrong host, an
	// outage — and decoding its HTML as JSON would hide that behind a parse error.
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("POST %s: unexpected status %s", endpoint, resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxOAuthResponse)).Decode(out); err != nil {
		return fmt.Errorf("decode response from %s: %w", endpoint, err)
	}
	return nil
}

// oauthClient talks to the device/oauth and installation endpoints. It has an
// explicit timeout because http.DefaultClient has none: a hung proxy would
// otherwise stall a connect, an installation poll, or a token refresh forever.
var oauthClient = &http.Client{Timeout: 30 * time.Second}

// maxOAuthResponse caps how much of a response is decoded, so a misdirected
// request answering with a large page cannot be read into memory.
const maxOAuthResponse = 1 << 20

func deviceFlowError(code, desc string) error {
	if code == "device_flow_disabled" {
		return fmt.Errorf("GitHub reports device flow is not enabled for this App (device_flow_disabled); " +
			"enable device flow on the shared multirunner App, or use `multirunner connect --own-app`")
	}
	return fmt.Errorf("device flow error: %s", describeError(code, desc))
}

func describeError(code, desc string) string {
	if desc != "" {
		return fmt.Sprintf("%s: %s", code, desc)
	}
	return code
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

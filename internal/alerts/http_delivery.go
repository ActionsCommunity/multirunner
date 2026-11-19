package alerts

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type IPResolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

var blockedWebhookCIDRs = []string{
	"0.0.0.0/8",
	"100.64.0.0/10",
	"192.0.0.0/24",
	"192.0.2.0/24",
	"198.18.0.0/15",
	"198.51.100.0/24",
	"203.0.113.0/24",
	"240.0.0.0/4",
	"100::/64",
	"2001::/23",
	"2001:db8::/32",
}

type httpDeliveryAdapter struct {
	endpoint *url.URL
	secret   string
	webhook  bool
	client   *http.Client
	now      func() time.Time
}

func NewAiTextAdapter(rawURL string, resolver IPResolver) (DeliveryAdapter, error) {
	endpoint, err := parseDeliveryURL(rawURL, true)
	if err != nil {
		return nil, err
	}
	client, err := restrictedHTTPClient(endpoint, resolver, true)
	if err != nil {
		return nil, err
	}
	return &httpDeliveryAdapter{
		endpoint: endpoint, client: client, now: time.Now,
	}, nil
}

func NewWebhookAdapter(rawURL, secret string, resolver IPResolver) (DeliveryAdapter, error) {
	endpoint, err := parseDeliveryURL(rawURL, false)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(secret) == "" {
		return nil, errors.New("webhook signing secret is required")
	}
	client, err := restrictedHTTPClient(endpoint, resolver, false)
	if err != nil {
		return nil, err
	}
	return &httpDeliveryAdapter{
		endpoint: endpoint, secret: secret, webhook: true, client: client, now: time.Now,
	}, nil
}

func (a *httpDeliveryAdapter) Deliver(ctx context.Context, delivery Delivery) error {
	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, a.endpoint.String(), bytes.NewReader(delivery.Payload),
	)
	if err != nil {
		return &PermanentError{Err: fmt.Errorf("build notification request: %w", err)}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "multirunner-alerts/1")
	request.Header.Set("X-Multirunner-Delivery", delivery.ID)
	request.Header.Set("X-Multirunner-Alert", delivery.AlertID)
	if a.webhook {
		timestamp := strconv.FormatInt(a.now().UTC().Unix(), 10)
		mac := hmac.New(sha256.New, []byte(a.secret))
		_, _ = mac.Write([]byte(timestamp))
		_, _ = mac.Write([]byte("."))
		_, _ = mac.Write(delivery.Payload)
		request.Header.Set("X-Multirunner-Timestamp", timestamp)
		request.Header.Set(
			"X-Multirunner-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)),
		)
	}
	response, err := a.client.Do(request)
	if err != nil {
		return fmt.Errorf("send notification: %w", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	switch {
	case response.StatusCode >= 200 && response.StatusCode < 300:
		return nil
	case response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500:
		return fmt.Errorf("notification endpoint returned %s", response.Status)
	default:
		return &PermanentError{
			Err: fmt.Errorf("notification endpoint returned %s", response.Status),
		}
	}
}

func parseDeliveryURL(rawURL string, local bool) (*url.URL, error) {
	endpoint, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || endpoint.Host == "" || endpoint.User != nil {
		return nil, errors.New("notification endpoint must be an absolute URL without user info")
	}
	if strings.Contains(endpoint.Hostname(), "%") {
		return nil, errors.New("notification endpoint cannot use a zoned IP address")
	}
	if local {
		if endpoint.Scheme != "http" && endpoint.Scheme != "https" {
			return nil, errors.New("AiText endpoint must use HTTP or HTTPS")
		}
		host := endpoint.Hostname()
		ip := net.ParseIP(host)
		if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
			return nil, errors.New("AiText endpoint must use a fixed loopback host")
		}
	} else if endpoint.Scheme != "https" {
		return nil, errors.New("webhook endpoint must use HTTPS")
	}
	return endpoint, nil
}

func restrictedHTTPClient(
	endpoint *url.URL, resolver IPResolver, local bool,
) (*http.Client, error) {
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	host := endpoint.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		if err := validateResolvedAddress(ip, local); err != nil {
			return nil, err
		}
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy:                 nil,
		TLSClientConfig:       &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		IdleConnTimeout:       30 * time.Second,
		DisableCompression:    true,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			addressHost, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, fmt.Errorf("parse notification address: %w", err)
			}
			if !strings.EqualFold(strings.TrimSuffix(addressHost, "."), strings.TrimSuffix(host, ".")) {
				return nil, errors.New("notification redirect changed destination host")
			}
			addresses, err := resolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, fmt.Errorf("resolve notification endpoint: %w", err)
			}
			if len(addresses) == 0 {
				return nil, errors.New("notification endpoint resolved to no addresses")
			}
			for _, address := range addresses {
				if err := validateResolvedAddress(address.IP, local); err != nil {
					return nil, err
				}
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(addresses[0].IP.String(), port))
		},
	}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, nil
}

func validateResolvedAddress(ip net.IP, local bool) error {
	if ip == nil {
		return errors.New("notification endpoint resolved to an invalid address")
	}
	if local {
		if !ip.IsLoopback() {
			return errors.New("AiText endpoint resolved outside loopback")
		}
		return nil
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return fmt.Errorf("webhook endpoint resolved to blocked address %s", ip)
	}
	for _, value := range blockedWebhookCIDRs {
		_, network, _ := net.ParseCIDR(value)
		if network.Contains(ip) {
			return fmt.Errorf("webhook endpoint resolved to blocked address %s", ip)
		}
	}
	return nil
}

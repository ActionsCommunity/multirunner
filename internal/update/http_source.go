package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

const (
	maxMetadataBytes = 8 << 20
	maxRootRotations = 32
)

type HTTPSource struct {
	base   *url.URL
	client *http.Client
}

func NewHTTPSource(baseURL string, client *http.Client) (*HTTPSource, error) {
	base, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return nil, fmt.Errorf("parse update metadata URL: %w", err)
	}
	if base.Scheme != "https" || base.Host == "" || base.User != nil ||
		base.RawQuery != "" || base.Fragment != "" {
		return nil, errors.New("update metadata URL must be an HTTPS directory without credentials, query, or fragment")
	}
	if client == nil {
		client = &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("update redirects are not permitted")
			},
		}
	}
	return &HTTPSource{base: base, client: client}, nil
}

func (s *HTTPSource) Bundle(ctx context.Context, currentRootVersion int) (MetadataBundle, error) {
	var bundle MetadataBundle
	for next := currentRootVersion + 1; next <= currentRootVersion+maxRootRotations; next++ {
		data, status, err := s.fetch(ctx, strconv.Itoa(next)+".root.json", maxMetadataBytes)
		if err != nil {
			return MetadataBundle{}, err
		}
		if status == http.StatusNotFound {
			break
		}
		if status != http.StatusOK {
			return MetadataBundle{}, fmt.Errorf("fetch root metadata: HTTP %d", status)
		}
		bundle.RootUpdates = append(bundle.RootUpdates, data)
	}
	timestamp, status, err := s.fetch(ctx, "timestamp.json", maxMetadataBytes)
	if err != nil {
		return MetadataBundle{}, err
	}
	if status != http.StatusOK {
		return MetadataBundle{}, fmt.Errorf("fetch timestamp metadata: HTTP %d", status)
	}
	bundle.Timestamp = timestamp
	var timestampEnvelope Envelope[Timestamp]
	if err := jsonUnmarshalUntrusted(timestamp, &timestampEnvelope); err != nil {
		return MetadataBundle{}, fmt.Errorf("inspect timestamp metadata: %w", err)
	}
	snapshotMeta, ok := timestampEnvelope.Signed.Meta["snapshot.json"]
	if !ok || snapshotMeta.Version < 1 {
		return MetadataBundle{}, fmt.Errorf("%w: timestamp omits snapshot version", ErrIntegrity)
	}
	snapshotName := strconv.Itoa(snapshotMeta.Version) + ".snapshot.json"
	snapshot, status, err := s.fetch(ctx, snapshotName, maxMetadataBytes)
	if err != nil {
		return MetadataBundle{}, err
	}
	if status != http.StatusOK {
		return MetadataBundle{}, fmt.Errorf("fetch snapshot metadata: HTTP %d", status)
	}
	bundle.Snapshot = snapshot
	var snapshotEnvelope Envelope[Snapshot]
	if err := jsonUnmarshalUntrusted(snapshot, &snapshotEnvelope); err != nil {
		return MetadataBundle{}, fmt.Errorf("inspect snapshot metadata: %w", err)
	}
	targetsMeta, ok := snapshotEnvelope.Signed.Meta["targets.json"]
	if !ok || targetsMeta.Version < 1 {
		return MetadataBundle{}, fmt.Errorf("%w: snapshot omits targets version", ErrIntegrity)
	}
	targetsName := strconv.Itoa(targetsMeta.Version) + ".targets.json"
	targets, status, err := s.fetch(ctx, targetsName, maxMetadataBytes)
	if err != nil {
		return MetadataBundle{}, err
	}
	if status != http.StatusOK {
		return MetadataBundle{}, fmt.Errorf("fetch targets metadata: HTTP %d", status)
	}
	bundle.Targets = targets
	return bundle, nil
}

func (s *HTTPSource) OpenTarget(
	ctx context.Context,
	targetPath string,
	target TargetFile,
) (io.ReadCloser, error) {
	if err := validRepositoryPath(targetPath); err != nil {
		return nil, err
	}
	hash := target.Hashes["sha256"]
	if !validSHA256(hash) {
		return nil, ErrIntegrity
	}
	directory, file := path.Split(targetPath)
	consistentPath := path.Join(directory, hash+"."+file)
	targetURL, err := s.resolve(consistentPath)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL.String(), nil)
	if err != nil {
		return nil, err
	}
	response, err := s.client.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		return nil, fmt.Errorf("fetch update target: HTTP %d", response.StatusCode)
	}
	if response.ContentLength >= 0 && response.ContentLength != target.Length {
		_ = response.Body.Close()
		return nil, fmt.Errorf("%w: target content length mismatch", ErrIntegrity)
	}
	return response.Body, nil
}

func (s *HTTPSource) fetch(
	ctx context.Context,
	name string,
	limit int64,
) ([]byte, int, error) {
	if err := validRepositoryPath(name); err != nil {
		return nil, 0, err
	}
	resource, err := s.resolve(name)
	if err != nil {
		return nil, 0, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, resource.String(), nil)
	if err != nil {
		return nil, 0, err
	}
	response, err := s.client.Do(request)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, response.StatusCode, nil
	}
	if response.ContentLength > limit {
		return nil, 0, fmt.Errorf("%w: metadata exceeds size limit", ErrIntegrity)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, 0, err
	}
	if int64(len(data)) > limit {
		return nil, 0, fmt.Errorf("%w: metadata exceeds size limit", ErrIntegrity)
	}
	return data, response.StatusCode, nil
}

func (s *HTTPSource) resolve(name string) (*url.URL, error) {
	relative, err := url.Parse(name)
	if err != nil {
		return nil, err
	}
	base := *s.base
	if !strings.HasSuffix(base.Path, "/") {
		base.Path += "/"
	}
	resolved := base.ResolveReference(relative)
	if resolved.Scheme != s.base.Scheme || resolved.Host != s.base.Host ||
		!strings.HasPrefix(resolved.EscapedPath(), base.EscapedPath()) {
		return nil, errors.New("update repository path escapes configured origin")
	}
	return resolved, nil
}

func validRepositoryPath(value string) error {
	if value == "" || strings.Contains(value, "\\") || strings.HasPrefix(value, "/") {
		return ErrIntegrity
	}
	clean := path.Clean(value)
	if clean == "." || clean != value || strings.HasPrefix(clean, "../") {
		return ErrIntegrity
	}
	return nil
}

func jsonUnmarshalUntrusted(data []byte, target any) error {
	if err := rejectDuplicateKeys(data); err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}

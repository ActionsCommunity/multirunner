package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ConnectHints holds the fields `multirunner connect` reads from an existing
// config to shape the GitHub App manifest.
type ConnectHints struct {
	Provisioning  Provisioning
	WebhookListen string
	GitHubURL     string
	Pools         int
}

// ReadConnectHints reads provisioning and webhook.listen from an existing config
// without validating it. A missing or unparseable file yields the zero value and
// ok=false, so connect can fall back to safe defaults rather than failing.
func ReadConnectHints(path string) (hints ConnectHints, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ConnectHints{}, false
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return ConnectHints{}, false
	}
	return ConnectHints{
		Provisioning:  c.Provisioning,
		WebhookListen: c.Webhook.Listen,
		GitHubURL:     c.GitHub.URL,
		Pools:         len(c.Pools),
	}, true
}

// WriteAppAuth updates (or creates) a config file's github + auth sections to use
// GitHub App credentials, preserving other content and comments. Any existing
// auth.pat is removed.
func WriteAppAuth(path string, scope Scope, owner, repo string, appID, installationID int64, keyPath string) error {
	doc, err := loadOrNewMapping(path)
	if err != nil {
		return err
	}

	gh := upsertMapping(doc, "github")
	setScalarIfAbsent(gh, "url", "https://github.com")
	setScalar(gh, "scope", string(scope))
	setScalar(gh, "owner", owner)
	if scope == ScopeRepo {
		setScalar(gh, "repo", repo)
	}

	auth := upsertMapping(doc, "auth")
	removeKey(auth, "pat")
	// Installation credentials supersede a device-flow login; leaving these
	// behind would keep a dormant user token wired into the config.
	removeKey(auth, "client_id")
	removeKey(auth, "token_path")
	setInt(auth, "app_id", appID)
	setInt(auth, "installation_id", installationID)
	setScalar(auth, "private_key_path", keyPath)

	return renderConfig(path, doc)
}

// WriteDeviceAuth updates (or creates) a config file's github + auth sections to
// use GitHub App device-flow credentials: a client id and a token-store path.
// Any existing auth.pat and installation-App keys (app_id, installation_id,
// private_key_path) are removed, since a user access token supersedes them.
func WriteDeviceAuth(path string, scope Scope, owner, repo, clientID, tokenPath string) error {
	doc, err := loadOrNewMapping(path)
	if err != nil {
		return err
	}

	gh := upsertMapping(doc, "github")
	setScalarIfAbsent(gh, "url", "https://github.com")
	setScalar(gh, "scope", string(scope))
	setScalar(gh, "owner", owner)
	if scope == ScopeRepo {
		setScalar(gh, "repo", repo)
	}

	auth := upsertMapping(doc, "auth")
	removeKey(auth, "pat")
	removeKey(auth, "app_id")
	removeKey(auth, "installation_id")
	removeKey(auth, "private_key_path")
	setScalar(auth, "client_id", clientID)
	setScalar(auth, "token_path", tokenPath)

	return renderConfig(path, doc)
}

// renderConfig writes doc back to path.
func renderConfig(path string, doc *yaml.Node) error {
	out, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, out)
}

// EnsureStarterPool appends a working first pool to a config that has none, and
// reports whether it added one. connect otherwise writes credentials only, which
// leaves a config that cannot run; a live pool means the next command does
// something, and its comments name the options so nothing has to be looked up.
//
// dockerHost is the endpoint the caller discovered on this machine. It is passed
// in rather than probed here so this package keeps no opinion about daemons.
func EnsureStarterPool(path, dockerHost string) (bool, error) {
	doc, err := loadOrNewMapping(path)
	if err != nil {
		return false, err
	}
	if v, _ := findKey(doc, "pools"); v != nil {
		return false, nil
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return false, err
	}
	// An empty mapping marshals to "{}", which is valid YAML but reads as
	// leftover noise at the top of a config a person is about to edit.
	if strings.TrimSpace(string(out)) == "{}" {
		out = nil
	}
	out = append(out, PoolsYAML(dockerHost)...)
	if err := writeFileAtomic(path, out); err != nil {
		return false, err
	}
	return true, nil
}

// loadOrNewMapping returns the document root of an existing config, or a fresh
// mapping when there is no config yet. Only a genuinely missing file yields a new
// mapping: an unreadable, malformed or non-mapping config is an error, because
// the caller rewrites the file and treating those cases as "absent" silently
// destroys a config the user still has.
func loadOrNewMapping(path string) (*yaml.Node, error) {
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return &yaml.Node{Kind: yaml.MappingNode}, nil
	case err != nil:
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parse config %s: %w (fix or move it, then re-run)", path, err)
	}
	// An empty file parses to a document with no content; that is a config the
	// user has not filled in yet, so starting from a fresh mapping is safe.
	if len(root.Content) == 0 {
		return &yaml.Node{Kind: yaml.MappingNode}, nil
	}
	if len(root.Content) != 1 || root.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("config %s is not a YAML mapping; refusing to overwrite it", path)
	}
	return root.Content[0], nil
}

// writeFileAtomic replaces path in one step, so a failure part-way through
// cannot leave a truncated config behind.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".mr-config-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp config in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // a no-op once the rename below succeeds

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp config: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp config: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace config %s: %w", path, err)
	}
	return nil
}

// findKey returns the value node and its key index for key, or (nil, -1).
func findKey(m *yaml.Node, key string) (*yaml.Node, int) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1], i
		}
	}
	return nil, -1
}

func upsertMapping(m *yaml.Node, key string) *yaml.Node {
	if v, _ := findKey(m, key); v != nil {
		if v.Kind != yaml.MappingNode {
			v.Kind = yaml.MappingNode
			v.Tag = "!!map"
			v.Content = nil
		}
		return v
	}
	v := &yaml.Node{Kind: yaml.MappingNode}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key}, v)
	return v
}

func setScalar(m *yaml.Node, key, value string) {
	if v, _ := findKey(m, key); v != nil {
		v.Kind = yaml.ScalarNode
		v.Tag = "!!str"
		v.Value = value
		return
	}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value})
}

func setInt(m *yaml.Node, key string, value int64) {
	v := fmt.Sprintf("%d", value)
	if n, _ := findKey(m, key); n != nil {
		n.Kind = yaml.ScalarNode
		n.Tag = "!!int"
		n.Value = v
		return
	}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: v})
}

func setScalarIfAbsent(m *yaml.Node, key, value string) {
	if v, _ := findKey(m, key); v == nil {
		setScalar(m, key, value)
	}
}

func removeKey(m *yaml.Node, key string) {
	if _, idx := findKey(m, key); idx >= 0 {
		m.Content = append(m.Content[:idx], m.Content[idx+2:]...)
	}
}

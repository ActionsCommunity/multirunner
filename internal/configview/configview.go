// Package configview exposes a redacted, read-only view of the loaded
// configuration and detects drift from the source file.
package configview

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/GerardSmit/multirunner/internal/config"
	"gopkg.in/yaml.v3"
)

type Field struct {
	Path            string `json:"path"`
	Effective       any    `json:"effective"`
	Default         any    `json:"default,omitempty"`
	Source          string `json:"source"`
	Secret          bool   `json:"secret"`
	RestartRequired bool   `json:"restart_required"`
}

type Snapshot struct {
	SourceFile         string    `json:"source_file"`
	LoadedAt           time.Time `json:"loaded_at"`
	InspectedAt        time.Time `json:"inspected_at"`
	StartupSHA256      string    `json:"startup_sha256"`
	CurrentSHA256      string    `json:"current_sha256"`
	Drifted            bool      `json:"drifted"`
	ChangedPaths       []string  `json:"changed_paths"`
	ValidationError    string    `json:"validation_error,omitempty"`
	RawRedacted        string    `json:"raw_redacted"`
	NormalizedRedacted string    `json:"normalized_redacted"`
	Fields             []Field   `json:"fields"`
	Guidance           string    `json:"guidance"`
}

type Inspector struct {
	path              string
	loadedAt          time.Time
	startupRaw        []byte
	startupHash       string
	startupFileValues map[string]any
	effectiveValues   map[string]any
	explicitPaths     map[string]struct{}
	rawRedacted       string
	normalized        string
}

func New(path string, loaded *config.Config) (*Inspector, error) {
	if loaded == nil {
		return nil, errors.New("loaded configuration is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve configuration path: %w", err)
	}
	raw, err := os.ReadFile(absolute)
	if err != nil {
		return nil, fmt.Errorf("read configuration: %w", err)
	}
	startupValues, explicit, redactedRaw, err := parseRedacted(raw)
	if err != nil {
		return nil, err
	}
	normalizedRaw, err := yaml.Marshal(loaded)
	if err != nil {
		return nil, fmt.Errorf("encode normalized configuration: %w", err)
	}
	effectiveValues, _, normalized, err := parseRedacted(normalizedRaw)
	if err != nil {
		return nil, err
	}
	return &Inspector{
		path: absolute, loadedAt: time.Now().UTC(), startupRaw: append([]byte(nil), raw...),
		startupHash: hash(raw), startupFileValues: startupValues,
		effectiveValues: effectiveValues, explicitPaths: explicit,
		rawRedacted: redactedRaw, normalized: normalized,
	}, nil
}

func (i *Inspector) Snapshot() (Snapshot, error) {
	current, err := os.ReadFile(i.path)
	if err != nil {
		return Snapshot{}, fmt.Errorf("read current configuration: %w", err)
	}
	currentValues, _, _, parseErr := parseRedacted(current)
	validationError := ""
	if parseErr != nil {
		validationError = parseErr.Error()
		currentValues = map[string]any{}
	} else if _, err := config.Load(i.path); err != nil {
		validationError = err.Error()
	}
	changed := changedPaths(i.startupFileValues, currentValues)
	fields := make([]Field, 0, len(i.effectiveValues))
	paths := make([]string, 0, len(i.effectiveValues))
	for path := range i.effectiveValues {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		value := i.effectiveValues[path]
		_, explicit := i.explicitPaths[path]
		source := "default"
		var defaultValue any = value
		if explicit {
			source = "file"
			defaultValue = nil
		}
		if isSecretPath(path) {
			source = "redacted"
			value = "<redacted>"
			defaultValue = nil
		}
		fields = append(fields, Field{
			Path: path, Effective: value, Default: defaultValue, Source: source,
			Secret: isSecretPath(path), RestartRequired: true,
		})
	}
	return Snapshot{
		SourceFile: i.path, LoadedAt: i.loadedAt, InspectedAt: time.Now().UTC(),
		StartupSHA256: i.startupHash, CurrentSHA256: hash(current),
		Drifted: i.startupHash != hash(current), ChangedPaths: changed,
		ValidationError: validationError, RawRedacted: i.rawRedacted,
		NormalizedRedacted: i.normalized, Fields: fields,
		Guidance: "Edit the source YAML on this host, run `multirunner doctor`, then restart the service.",
	}, nil
}

func parseRedacted(raw []byte) (map[string]any, map[string]struct{}, string, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return nil, nil, "", fmt.Errorf("parse configuration YAML: %w", err)
	}
	if len(root.Content) == 0 {
		return map[string]any{}, map[string]struct{}{}, "", nil
	}
	redactNode(root.Content[0], nil)
	encoded, err := yaml.Marshal(root.Content[0])
	if err != nil {
		return nil, nil, "", fmt.Errorf("encode redacted configuration: %w", err)
	}
	values := make(map[string]any)
	explicit := make(map[string]struct{})
	flattenNode(root.Content[0], nil, values, explicit)
	return values, explicit, string(encoded), nil
}

func redactNode(node *yaml.Node, path []string) {
	switch node.Kind {
	case yaml.MappingNode:
		for index := 0; index+1 < len(node.Content); index += 2 {
			key := node.Content[index].Value
			value := node.Content[index+1]
			childPath := appendPath(path, key)
			if isSecretPath(strings.Join(childPath, ".")) {
				value.Kind = yaml.ScalarNode
				value.Tag = "!!str"
				value.Value = "<redacted>"
				value.Content = nil
				continue
			}
			redactNode(value, childPath)
		}
	case yaml.SequenceNode:
		for _, child := range node.Content {
			redactNode(child, path)
		}
	}
}

func flattenNode(
	node *yaml.Node, path []string, values map[string]any,
	explicit map[string]struct{},
) {
	switch node.Kind {
	case yaml.MappingNode:
		for index := 0; index+1 < len(node.Content); index += 2 {
			key := node.Content[index].Value
			flattenNode(node.Content[index+1], appendPath(path, key), values, explicit)
		}
	case yaml.SequenceNode:
		for index, child := range node.Content {
			flattenNode(child, appendPath(path, fmt.Sprintf("%d", index)), values, explicit)
		}
	case yaml.ScalarNode:
		key := strings.Join(path, ".")
		var value any
		if err := node.Decode(&value); err != nil {
			value = node.Value
		}
		values[key] = value
		explicit[key] = struct{}{}
	}
}

func changedPaths(before, after map[string]any) []string {
	keys := make(map[string]struct{}, len(before)+len(after))
	for key := range before {
		keys[key] = struct{}{}
	}
	for key := range after {
		keys[key] = struct{}{}
	}
	var changed []string
	for key := range keys {
		if fmt.Sprint(before[key]) != fmt.Sprint(after[key]) {
			changed = append(changed, key)
		}
	}
	sort.Strings(changed)
	return changed
}

func appendPath(path []string, value string) []string {
	result := make([]string, len(path), len(path)+1)
	copy(result, path)
	return append(result, value)
}

func isSecretPath(path string) bool {
	path = strings.ToLower(path)
	switch path {
	case "auth.pat", "webhook.secret", "cache.access_token":
		return true
	}
	return strings.HasPrefix(path, "history.notifications.webhooks.") &&
		strings.HasSuffix(path, ".secret")
}

func hash(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

package update

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
)

func Verify(
	trustedRoot []byte,
	bundle MetadataBundle,
	state TrustedState,
	installed Installed,
	policy Policy,
	now time.Time,
) (Verified, error) {
	if err := validatePolicy(policy); err != nil {
		return Verified{}, err
	}
	root, err := decodeEnvelope[Root](trustedRoot)
	if err != nil {
		return Verified{}, err
	}
	if err := validateRoot(root.Signed, now); err != nil {
		return Verified{}, err
	}
	if err := verifyRole(root, "root", root.Signatures, root.Signed, policy); err != nil {
		return Verified{}, fmt.Errorf("verify trusted root: %w", err)
	}
	if state.RootVersion > 0 && root.Signed.Version != state.RootVersion {
		return Verified{}, fmt.Errorf("%w: trusted root version %d does not match state %d",
			ErrRollback, root.Signed.Version, state.RootVersion)
	}

	for index, raw := range bundle.RootUpdates {
		next, decodeErr := decodeEnvelope[Root](raw)
		if decodeErr != nil {
			return Verified{}, fmt.Errorf("decode root update %d: %w", index+1, decodeErr)
		}
		if next.Signed.Version != root.Signed.Version+1 {
			return Verified{}, fmt.Errorf("%w: root version %d must follow %d",
				ErrRollback, next.Signed.Version, root.Signed.Version)
		}
		if err := validateRoot(next.Signed, now); err != nil {
			return Verified{}, err
		}
		if err := verifyRole(root, "root", next.Signatures, next.Signed, policy); err != nil {
			return Verified{}, fmt.Errorf("verify root %d with prior root: %w", next.Signed.Version, err)
		}
		if err := verifyRole(next, "root", next.Signatures, next.Signed, policy); err != nil {
			return Verified{}, fmt.Errorf("verify root %d with new root: %w", next.Signed.Version, err)
		}
		root = next
	}

	timestamp, err := decodeEnvelope[Timestamp](bundle.Timestamp)
	if err != nil {
		return Verified{}, fmt.Errorf("decode timestamp: %w", err)
	}
	if err := validateMetadata("timestamp", timestamp.Signed.Type, timestamp.Signed.SpecVersion,
		timestamp.Signed.Version, state.TimestampVersion, timestamp.Signed.Expires, now); err != nil {
		return Verified{}, err
	}
	if err := verifyRole(root, "timestamp", timestamp.Signatures, timestamp.Signed, policy); err != nil {
		return Verified{}, fmt.Errorf("verify timestamp: %w", err)
	}

	snapshotMeta, ok := timestamp.Signed.Meta["snapshot.json"]
	if !ok {
		return Verified{}, fmt.Errorf("%w: timestamp omits snapshot.json", ErrIntegrity)
	}
	if err := verifyLinked("snapshot.json", bundle.Snapshot, snapshotMeta); err != nil {
		return Verified{}, err
	}
	snapshot, err := decodeEnvelope[Snapshot](bundle.Snapshot)
	if err != nil {
		return Verified{}, fmt.Errorf("decode snapshot: %w", err)
	}
	if snapshot.Signed.Version != snapshotMeta.Version {
		return Verified{}, fmt.Errorf("%w: snapshot version does not match timestamp", ErrIntegrity)
	}
	if err := validateMetadata("snapshot", snapshot.Signed.Type, snapshot.Signed.SpecVersion,
		snapshot.Signed.Version, state.SnapshotVersion, snapshot.Signed.Expires, now); err != nil {
		return Verified{}, err
	}
	if err := verifyRole(root, "snapshot", snapshot.Signatures, snapshot.Signed, policy); err != nil {
		return Verified{}, fmt.Errorf("verify snapshot: %w", err)
	}

	targetsMeta, ok := snapshot.Signed.Meta["targets.json"]
	if !ok {
		return Verified{}, fmt.Errorf("%w: snapshot omits targets.json", ErrIntegrity)
	}
	if err := verifyLinked("targets.json", bundle.Targets, targetsMeta); err != nil {
		return Verified{}, err
	}
	targets, err := decodeEnvelope[Targets](bundle.Targets)
	if err != nil {
		return Verified{}, fmt.Errorf("decode targets: %w", err)
	}
	if targets.Signed.Version != targetsMeta.Version {
		return Verified{}, fmt.Errorf("%w: targets version does not match snapshot", ErrIntegrity)
	}
	if err := validateMetadata("targets", targets.Signed.Type, targets.Signed.SpecVersion,
		targets.Signed.Version, state.TargetsVersion, targets.Signed.Expires, now); err != nil {
		return Verified{}, err
	}
	if err := verifyRole(root, "targets", targets.Signatures, targets.Signed, policy); err != nil {
		return Verified{}, fmt.Errorf("verify targets: %w", err)
	}

	targetPath, target, err := selectTarget(targets.Signed.Targets, installed, policy)
	if err != nil {
		return Verified{}, err
	}
	return Verified{
		Root: root, Timestamp: timestamp, Snapshot: snapshot, Targets: targets,
		TargetPath: targetPath, Target: target,
		NextState: TrustedState{
			RootVersion: root.Signed.Version, TimestampVersion: timestamp.Signed.Version,
			SnapshotVersion: snapshot.Signed.Version, TargetsVersion: targets.Signed.Version,
		},
		MetadataRaw: map[string]json.RawMessage{
			"timestamp.json": append(json.RawMessage(nil), bundle.Timestamp...),
			"snapshot.json":  append(json.RawMessage(nil), bundle.Snapshot...),
			"targets.json":   append(json.RawMessage(nil), bundle.Targets...),
		},
	}, nil
}

func decodeEnvelope[T any](data []byte) (Envelope[T], error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return Envelope[T]{}, fmt.Errorf("%w: metadata is empty", ErrIntegrity)
	}
	if err := rejectDuplicateKeys(data); err != nil {
		return Envelope[T]{}, err
	}
	var envelope Envelope[T]
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&envelope); err != nil {
		return Envelope[T]{}, fmt.Errorf("%w: %v", ErrIntegrity, err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return Envelope[T]{}, fmt.Errorf("%w: trailing JSON data", ErrIntegrity)
	}
	if len(envelope.Signatures) == 0 {
		return Envelope[T]{}, fmt.Errorf("%w: metadata has no signatures", ErrUntrusted)
	}
	return envelope, nil
}

func rejectDuplicateKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var walk func() error
	walk = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]struct{}{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return fmt.Errorf("object key is not a string")
				}
				if _, exists := seen[key]; exists {
					return fmt.Errorf("duplicate object key %q", key)
				}
				seen[key] = struct{}{}
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		case '[':
			for decoder.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		default:
			return nil
		}
	}
	if err := walk(); err != nil {
		return fmt.Errorf("%w: invalid JSON: %v", ErrIntegrity, err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return fmt.Errorf("%w: trailing JSON data", ErrIntegrity)
	}
	return nil
}

func validateRoot(root Root, now time.Time) error {
	if root.Type != "root" || root.SpecVersion != SpecVersion || root.Version < 1 {
		return fmt.Errorf("%w: invalid root metadata", ErrIntegrity)
	}
	if !root.Expires.After(now) {
		return fmt.Errorf("%w: root expired at %s", ErrExpired, root.Expires.UTC().Format(time.RFC3339))
	}
	for _, name := range []string{"root", "timestamp", "snapshot", "targets"} {
		role, ok := root.Roles[name]
		if !ok || role.Threshold < 1 || role.Threshold > len(role.KeyIDs) {
			return fmt.Errorf("%w: invalid %s role", ErrIntegrity, name)
		}
		seen := map[string]struct{}{}
		for _, keyID := range role.KeyIDs {
			if _, duplicate := seen[keyID]; duplicate {
				return fmt.Errorf("%w: duplicate %s role key", ErrIntegrity, name)
			}
			seen[keyID] = struct{}{}
			key, exists := root.Keys[keyID]
			if !exists || key.KeyType != "ed25519" || key.Scheme != "ed25519" {
				return fmt.Errorf("%w: invalid %s role key %q", ErrIntegrity, name, keyID)
			}
			public, err := hex.DecodeString(key.KeyVal.Public)
			if err != nil || len(public) != ed25519.PublicKeySize || keyIDFor(public) != keyID {
				return fmt.Errorf("%w: invalid key %q", ErrIntegrity, keyID)
			}
		}
	}
	return nil
}

func validateMetadata(
	name, metadataType, spec string,
	version, priorVersion int,
	expires, now time.Time,
) error {
	if metadataType != name || spec != SpecVersion || version < 1 {
		return fmt.Errorf("%w: invalid %s metadata", ErrIntegrity, name)
	}
	if version < priorVersion {
		return fmt.Errorf("%w: %s version %d is older than %d",
			ErrRollback, name, version, priorVersion)
	}
	if !expires.After(now) {
		return fmt.Errorf("%w: %s expired at %s",
			ErrExpired, name, expires.UTC().Format(time.RFC3339))
	}
	return nil
}

func verifyRole[T any](
	root Envelope[Root],
	roleName string,
	signatures []Signature,
	signed T,
	policy Policy,
) error {
	role, ok := root.Signed.Roles[roleName]
	if !ok {
		return fmt.Errorf("%w: root omits %s role", ErrUntrusted, roleName)
	}
	payload, err := canonicalJSON(signed)
	if err != nil {
		return fmt.Errorf("%w: canonicalize signed metadata: %v", ErrIntegrity, err)
	}
	revoked := stringSet(policy.RevokedKeyIDs)
	allowedTargets := stringSet(policy.AllowedTargetKeyIDs)
	valid := map[string]struct{}{}
	for _, signature := range signatures {
		if _, denied := revoked[signature.KeyID]; denied {
			return fmt.Errorf("%w: signature uses revoked key %q", ErrUntrusted, signature.KeyID)
		}
		if roleName == "targets" && len(allowedTargets) > 0 {
			if _, allowed := allowedTargets[signature.KeyID]; !allowed {
				continue
			}
		}
		if _, counted := valid[signature.KeyID]; counted {
			continue
		}
		if !contains(role.KeyIDs, signature.KeyID) {
			continue
		}
		key := root.Signed.Keys[signature.KeyID]
		public, publicErr := hex.DecodeString(key.KeyVal.Public)
		rawSignature, signatureErr := hex.DecodeString(signature.Sig)
		if publicErr != nil || signatureErr != nil ||
			len(public) != ed25519.PublicKeySize || len(rawSignature) != ed25519.SignatureSize {
			continue
		}
		if ed25519.Verify(public, payload, rawSignature) {
			valid[signature.KeyID] = struct{}{}
		}
	}
	if len(valid) < role.Threshold {
		return fmt.Errorf("%w: %s requires %d trusted signatures, got %d",
			ErrUntrusted, roleName, role.Threshold, len(valid))
	}
	return nil
}

func verifyLinked(name string, data []byte, meta FileMeta) error {
	if meta.Version < 1 || meta.Length < 1 || int64(len(data)) != meta.Length {
		return fmt.Errorf("%w: %s length/version mismatch", ErrIntegrity, name)
	}
	expected, ok := meta.Hashes["sha256"]
	if !ok || !validSHA256(expected) {
		return fmt.Errorf("%w: %s has no valid sha256", ErrIntegrity, name)
	}
	actual := sha256.Sum256(data)
	if !strings.EqualFold(expected, hex.EncodeToString(actual[:])) {
		return fmt.Errorf("%w: %s sha256 mismatch", ErrIntegrity, name)
	}
	return nil
}

func selectTarget(
	targets map[string]TargetFile,
	installed Installed,
	policy Policy,
) (string, TargetFile, error) {
	var selectedPath string
	var selected TargetFile
	for path, target := range targets {
		if target.Custom.OS != installed.OS || target.Custom.Arch != installed.Arch {
			continue
		}
		if selectedPath != "" {
			return "", TargetFile{}, fmt.Errorf("%w: multiple targets match %s/%s",
				ErrIntegrity, installed.OS, installed.Arch)
		}
		selectedPath, selected = path, target
	}
	if selectedPath == "" {
		return "", TargetFile{}, fmt.Errorf("%w for %s/%s", ErrNotFound, installed.OS, installed.Arch)
	}
	custom := selected.Custom
	if selected.Length < 1 || !validSHA256(selected.Hashes["sha256"]) ||
		!validReleaseVersion(custom.Version) || !validCommit(custom.Commit) ||
		custom.Provenance.Commit != custom.Commit ||
		custom.APIVersion == "" || custom.APIVersion != installed.APIVersion ||
		custom.SchemaMin < 1 || custom.SchemaMax < custom.SchemaMin ||
		installed.SchemaVersion < custom.SchemaMin || installed.SchemaVersion > custom.SchemaMax ||
		!validSHA256(custom.EmbeddedConsoleSHA256) ||
		!validSHA256(custom.SBOMSHA256) || !validSHA256(custom.ProvenanceSHA256) {
		return "", TargetFile{}, fmt.Errorf("%w: target metadata is incomplete or incompatible", ErrIncompatible)
	}
	if compareReleaseVersions(custom.Version, installed.Version) <= 0 {
		return "", TargetFile{}, fmt.Errorf("%w: target %s does not advance installed %s",
			ErrRollback, custom.Version, installed.Version)
	}
	if installed.Commit != "" && custom.Commit == installed.Commit {
		return "", TargetFile{}, fmt.Errorf("%w: target commit matches installed build", ErrRollback)
	}
	if policy.Repository == "" || policy.Workflow == "" || policy.BuilderID == "" ||
		custom.Provenance.Repository != policy.Repository ||
		custom.Provenance.Workflow != policy.Workflow ||
		custom.Provenance.BuilderID != policy.BuilderID {
		return "", TargetFile{}, fmt.Errorf("%w: target provenance policy mismatch", ErrUntrusted)
	}
	return selectedPath, selected, nil
}

func VerifyArtifact(reader io.Reader, target TargetFile) error {
	if reader == nil || target.Length < 1 || !validSHA256(target.Hashes["sha256"]) {
		return ErrIntegrity
	}
	hash := sha256.New()
	count, err := io.Copy(hash, io.LimitReader(reader, target.Length+1))
	if err != nil {
		return fmt.Errorf("hash update artifact: %w", err)
	}
	if count != target.Length {
		return fmt.Errorf("%w: artifact length is %d, expected %d", ErrIntegrity, count, target.Length)
	}
	if !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), target.Hashes["sha256"]) {
		return fmt.Errorf("%w: artifact sha256 mismatch", ErrIntegrity)
	}
	return nil
}

func keyIDFor(public []byte) string {
	key := Key{KeyType: "ed25519", Scheme: "ed25519", KeyVal: KeyValue{Public: hex.EncodeToString(public)}}
	canonical, _ := canonicalJSON(key)
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:])
}

func canonicalJSON(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, err
	}
	var output bytes.Buffer
	if err := writeCanonicalJSON(&output, decoded); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func writeCanonicalJSON(output *bytes.Buffer, value any) error {
	switch typed := value.(type) {
	case nil:
		output.WriteString("null")
	case bool:
		output.WriteString(strconv.FormatBool(typed))
	case string:
		writeJSONString(output, typed)
	case json.Number:
		if strings.ContainsAny(string(typed), ".eE") {
			return fmt.Errorf("canonical metadata does not permit non-integer numbers")
		}
		if _, err := strconv.ParseInt(string(typed), 10, 64); err != nil {
			return fmt.Errorf("invalid canonical integer %q", typed)
		}
		output.WriteString(string(typed))
	case []any:
		output.WriteByte('[')
		for index, item := range typed {
			if index > 0 {
				output.WriteByte(',')
			}
			if err := writeCanonicalJSON(output, item); err != nil {
				return err
			}
		}
		output.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		output.WriteByte('{')
		for index, key := range keys {
			if index > 0 {
				output.WriteByte(',')
			}
			writeJSONString(output, key)
			output.WriteByte(':')
			if err := writeCanonicalJSON(output, typed[key]); err != nil {
				return err
			}
		}
		output.WriteByte('}')
	default:
		return fmt.Errorf("unsupported canonical JSON value %T", value)
	}
	return nil
}

func writeJSONString(output *bytes.Buffer, value string) {
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
	output.Write(bytes.TrimSuffix(encoded.Bytes(), []byte{'\n'}))
}

func validatePolicy(policy Policy) error {
	if len(policy.AllowedTargetKeyIDs) == 0 ||
		strings.TrimSpace(policy.Repository) == "" ||
		strings.TrimSpace(policy.Workflow) == "" ||
		strings.TrimSpace(policy.BuilderID) == "" {
		return fmt.Errorf("%w: signing identity and provenance policy are required", ErrUntrusted)
	}
	allowed := stringSet(policy.AllowedTargetKeyIDs)
	if len(allowed) != len(policy.AllowedTargetKeyIDs) {
		return fmt.Errorf("%w: duplicate allowed target key", ErrUntrusted)
	}
	for keyID := range allowed {
		if !validSHA256(keyID) {
			return fmt.Errorf("%w: invalid allowed target key ID", ErrUntrusted)
		}
	}
	for _, keyID := range policy.RevokedKeyIDs {
		if !validSHA256(keyID) {
			return fmt.Errorf("%w: invalid revoked key ID", ErrUntrusted)
		}
		if _, conflict := allowed[keyID]; conflict {
			return fmt.Errorf("%w: target key is both allowed and revoked", ErrUntrusted)
		}
	}
	return nil
}

func validSHA256(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func validCommit(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && (len(decoded) == 20 || len(decoded) == 32)
}

func validReleaseVersion(value string) bool {
	_, ok := releaseVersionParts(value)
	return ok
}

func compareReleaseVersions(left, right string) int {
	leftParts, leftOK := releaseVersionParts(left)
	rightParts, rightOK := releaseVersionParts(right)
	if !leftOK || !rightOK {
		return strings.Compare(left, right)
	}
	for index := range leftParts {
		if leftParts[index] < rightParts[index] {
			return -1
		}
		if leftParts[index] > rightParts[index] {
			return 1
		}
	}
	return 0
}

func releaseVersionParts(value string) ([3]uint64, bool) {
	value = strings.TrimPrefix(strings.TrimSpace(value), "v")
	if strings.ContainsAny(value, "-+") {
		return [3]uint64{}, false
	}
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return [3]uint64{}, false
	}
	var result [3]uint64
	for index, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return [3]uint64{}, false
		}
		number, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return [3]uint64{}, false
		}
		result[index] = number
	}
	return result, true
}

func stringSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

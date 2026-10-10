package update

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

type testSigner struct {
	id      string
	public  ed25519.PublicKey
	private ed25519.PrivateKey
}

type testRepository struct {
	now       time.Time
	root      []byte
	timestamp []byte
	snapshot  []byte
	targets   []byte
	artifact  []byte
	signers   map[string]testSigner
	installed Installed
	policy    Policy
}

func TestVerifyAcceptsThresholdSignedLinkedMetadata(t *testing.T) {
	repository := newTestRepository(t)
	verified, err := Verify(
		repository.root,
		MetadataBundle{
			Timestamp: repository.timestamp,
			Snapshot:  repository.snapshot,
			Targets:   repository.targets,
		},
		TrustedState{},
		repository.installed,
		repository.policy,
		repository.now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if verified.TargetPath != "multirunner_v1.2.0_windows_amd64.exe" {
		t.Fatalf("target path = %q", verified.TargetPath)
	}
	if verified.NextState != (TrustedState{
		RootVersion: 1, TimestampVersion: 3, SnapshotVersion: 4, TargetsVersion: 5,
	}) {
		t.Fatalf("state = %+v", verified.NextState)
	}
	if err := VerifyArtifact(strings.NewReader(string(repository.artifact)), verified.Target); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyRejectsTrustAndLinkageFailures(t *testing.T) {
	tests := map[string]func(*testRepository){
		"expired timestamp": func(repository *testRepository) {
			var envelope Envelope[Timestamp]
			mustJSON(t, repository.timestamp, &envelope)
			envelope.Signed.Expires = repository.now.Add(-time.Minute)
			repository.timestamp = signEnvelope(t, envelope.Signed, repository.signers["timestamp"])
		},
		"snapshot digest mismatch": func(repository *testRepository) {
			repository.snapshot[10] ^= 1
		},
		"metadata rollback": func(repository *testRepository) {
			var envelope Envelope[Timestamp]
			mustJSON(t, repository.timestamp, &envelope)
			envelope.Signed.Version = 2
			repository.timestamp = signEnvelope(t, envelope.Signed, repository.signers["timestamp"])
		},
		"untrusted targets signer": func(repository *testRepository) {
			rogue := newSigner(t)
			var envelope Envelope[Targets]
			mustJSON(t, repository.targets, &envelope)
			repository.targets = signEnvelope(t, envelope.Signed, rogue)
			repository.relink(t)
		},
		"revoked targets signer": func(repository *testRepository) {
			repository.policy.RevokedKeyIDs = []string{repository.signers["targets"].id}
		},
		"provenance mismatch": func(repository *testRepository) {
			var envelope Envelope[Targets]
			mustJSON(t, repository.targets, &envelope)
			target := envelope.Signed.Targets["multirunner_v1.2.0_windows_amd64.exe"]
			target.Custom.Provenance.Repository = "attacker/example"
			envelope.Signed.Targets["multirunner_v1.2.0_windows_amd64.exe"] = target
			repository.targets = signEnvelope(t, envelope.Signed, repository.signers["targets"])
			repository.relink(t)
		},
		"version downgrade": func(repository *testRepository) {
			var envelope Envelope[Targets]
			mustJSON(t, repository.targets, &envelope)
			target := envelope.Signed.Targets["multirunner_v1.2.0_windows_amd64.exe"]
			target.Custom.Version = "v1.0.0"
			envelope.Signed.Targets["multirunner_v1.2.0_windows_amd64.exe"] = target
			repository.targets = signEnvelope(t, envelope.Signed, repository.signers["targets"])
			repository.relink(t)
		},
		"schema incompatibility": func(repository *testRepository) {
			var envelope Envelope[Targets]
			mustJSON(t, repository.targets, &envelope)
			target := envelope.Signed.Targets["multirunner_v1.2.0_windows_amd64.exe"]
			target.Custom.SchemaMin = 13
			envelope.Signed.Targets["multirunner_v1.2.0_windows_amd64.exe"] = target
			repository.targets = signEnvelope(t, envelope.Signed, repository.signers["targets"])
			repository.relink(t)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			repository := newTestRepository(t)
			mutate(repository)
			_, err := Verify(
				repository.root,
				MetadataBundle{
					Timestamp: repository.timestamp,
					Snapshot:  repository.snapshot,
					Targets:   repository.targets,
				},
				TrustedState{TimestampVersion: 3},
				repository.installed,
				repository.policy,
				repository.now,
			)
			if err == nil {
				t.Fatal("verification unexpectedly succeeded")
			}
		})
	}
}

func TestVerifyRootRotationRequiresOldAndNewThresholds(t *testing.T) {
	repository := newTestRepository(t)
	oldRoot := decodeRoot(t, repository.root)
	newRootSigner := newSigner(t)
	nextRoot := oldRoot.Signed
	nextRoot.Version = 2
	nextRoot.Keys[newRootSigner.id] = signerKey(newRootSigner)
	nextRoot.Roles["root"] = Role{KeyIDs: []string{newRootSigner.id}, Threshold: 1}
	onlyNew := signEnvelope(t, nextRoot, newRootSigner)

	_, err := Verify(
		repository.root,
		MetadataBundle{
			RootUpdates: [][]byte{onlyNew},
			Timestamp:   repository.timestamp, Snapshot: repository.snapshot, Targets: repository.targets,
		},
		TrustedState{}, repository.installed, repository.policy, repository.now,
	)
	if !errors.Is(err, ErrUntrusted) {
		t.Fatalf("only-new root error = %v", err)
	}

	signedByBoth := signEnvelope(t, nextRoot, repository.signers["root"], newRootSigner)
	verified, err := Verify(
		repository.root,
		MetadataBundle{
			RootUpdates: [][]byte{signedByBoth},
			Timestamp:   repository.timestamp, Snapshot: repository.snapshot, Targets: repository.targets,
		},
		TrustedState{}, repository.installed, repository.policy, repository.now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Root.Signed.Version != 2 {
		t.Fatalf("root version = %d", verified.Root.Signed.Version)
	}
}

func TestVerifyRejectsDuplicateJSONKeys(t *testing.T) {
	repository := newTestRepository(t)
	duplicate := []byte(`{"signed":{},"signed":{},"signatures":[]}`)
	_, err := Verify(
		duplicate,
		MetadataBundle{},
		TrustedState{},
		repository.installed,
		repository.policy,
		repository.now,
	)
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("duplicate key error = %v", err)
	}
}

func TestVerifyArtifactRejectsLengthAndDigestMismatch(t *testing.T) {
	repository := newTestRepository(t)
	target := decodeTargets(t, repository.targets).
		Signed.Targets["multirunner_v1.2.0_windows_amd64.exe"]
	if err := VerifyArtifact(strings.NewReader("short"), target); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("short artifact error = %v", err)
	}
	target.Hashes["sha256"] = strings.Repeat("0", 64)
	if err := VerifyArtifact(strings.NewReader(string(repository.artifact)), target); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("digest error = %v", err)
	}
}

func newTestRepository(t *testing.T) *testRepository {
	t.Helper()
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	signers := map[string]testSigner{
		"root": newSigner(t), "timestamp": newSigner(t),
		"snapshot": newSigner(t), "targets": newSigner(t),
	}
	keys := map[string]Key{}
	roles := map[string]Role{}
	for _, name := range []string{"root", "timestamp", "snapshot", "targets"} {
		signer := signers[name]
		keys[signer.id] = signerKey(signer)
		roles[name] = Role{KeyIDs: []string{signer.id}, Threshold: 1}
	}
	root := signEnvelope(t, Root{
		Type: "root", SpecVersion: SpecVersion, Version: 1,
		Expires: now.Add(365 * 24 * time.Hour), ConsistentSnapshot: true,
		Keys: keys, Roles: roles,
	}, signers["root"])

	artifact := []byte("trusted executable bytes")
	artifactDigest := sha256.Sum256(artifact)
	digest := strings.Repeat("a", 64)
	commit := strings.Repeat("1", 40)
	targets := signEnvelope(t, Targets{
		Type: "targets", SpecVersion: SpecVersion, Version: 5,
		Expires: now.Add(24 * time.Hour),
		Targets: map[string]TargetFile{
			"multirunner_v1.2.0_windows_amd64.exe": {
				Length: int64(len(artifact)),
				Hashes: map[string]string{"sha256": hex.EncodeToString(artifactDigest[:])},
				Custom: TargetMetadata{
					Version: "v1.2.0", Commit: commit, OS: "windows", Arch: "amd64",
					APIVersion: "v1", SchemaMin: 12, SchemaMax: 12,
					EmbeddedConsoleSHA256: digest, SBOMSHA256: digest, ProvenanceSHA256: digest,
					Provenance: Provenance{
						Repository: "GerardSmit/multirunner", Commit: commit,
						Workflow:  ".github/workflows/release.yml",
						BuilderID: "https://github.com/actions/runner",
					},
				},
			},
		},
	}, signers["targets"])
	snapshot := signEnvelope(t, Snapshot{
		Type: "snapshot", SpecVersion: SpecVersion, Version: 4,
		Expires: now.Add(12 * time.Hour),
		Meta:    map[string]FileMeta{"targets.json": linkedMeta(5, targets)},
	}, signers["snapshot"])
	timestamp := signEnvelope(t, Timestamp{
		Type: "timestamp", SpecVersion: SpecVersion, Version: 3,
		Expires: now.Add(time.Hour),
		Meta:    map[string]FileMeta{"snapshot.json": linkedMeta(4, snapshot)},
	}, signers["timestamp"])
	return &testRepository{
		now: now, root: root, timestamp: timestamp, snapshot: snapshot, targets: targets,
		artifact: artifact, signers: signers,
		installed: Installed{
			Version: "v1.1.0", Commit: strings.Repeat("0", 40),
			OS: "windows", Arch: "amd64", APIVersion: "v1", SchemaVersion: 12,
		},
		policy: Policy{
			AllowedTargetKeyIDs: []string{signers["targets"].id},
			Repository:          "GerardSmit/multirunner",
			Workflow:            ".github/workflows/release.yml",
			BuilderID:           "https://github.com/actions/runner",
		},
	}
}

func (repository *testRepository) relink(t *testing.T) {
	t.Helper()
	var snapshot Envelope[Snapshot]
	mustJSON(t, repository.snapshot, &snapshot)
	snapshot.Signed.Meta["targets.json"] = linkedMeta(5, repository.targets)
	repository.snapshot = signEnvelope(t, snapshot.Signed, repository.signers["snapshot"])

	var timestamp Envelope[Timestamp]
	mustJSON(t, repository.timestamp, &timestamp)
	timestamp.Signed.Meta["snapshot.json"] = linkedMeta(4, repository.snapshot)
	repository.timestamp = signEnvelope(t, timestamp.Signed, repository.signers["timestamp"])
}

func newSigner(t *testing.T) testSigner {
	t.Helper()
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return testSigner{id: keyIDFor(public), public: public, private: private}
}

func signerKey(signer testSigner) Key {
	return Key{
		KeyType: "ed25519", Scheme: "ed25519",
		KeyVal: KeyValue{Public: hex.EncodeToString(signer.public)},
	}
}

func signEnvelope[T any](t *testing.T, signed T, signers ...testSigner) []byte {
	t.Helper()
	payload, err := canonicalJSON(signed)
	if err != nil {
		t.Fatal(err)
	}
	envelope := Envelope[T]{Signed: signed}
	for _, signer := range signers {
		envelope.Signatures = append(envelope.Signatures, Signature{
			KeyID: signer.id,
			Sig:   hex.EncodeToString(ed25519.Sign(signer.private, payload)),
		})
	}
	data, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func linkedMeta(version int, data []byte) FileMeta {
	digest := sha256.Sum256(data)
	return FileMeta{
		Version: version, Length: int64(len(data)),
		Hashes: map[string]string{"sha256": hex.EncodeToString(digest[:])},
	}
}

func decodeRoot(t *testing.T, data []byte) Envelope[Root] {
	t.Helper()
	var value Envelope[Root]
	mustJSON(t, data, &value)
	return value
}

func decodeTargets(t *testing.T, data []byte) Envelope[Targets] {
	t.Helper()
	var value Envelope[Targets]
	mustJSON(t, data, &value)
	return value
}

func mustJSON(t *testing.T, data []byte, target any) {
	t.Helper()
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatal(err)
	}
}

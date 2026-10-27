package update

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestPrepareAndVerifyFinalRepositoryEndToEnd(t *testing.T) {
	repository := newTestRepository(t)
	prepared := prepareTestRepository(t, repository)
	files := signPreparedRepository(t, repository, prepared)

	if err := VerifyPreparedRepository(repository.root, prepared); err != nil {
		t.Fatal(err)
	}
	if err := VerifyFinalRepositoryAgainstPrepared(repository.root, prepared, files); err != nil {
		t.Fatal(err)
	}
	verified, err := Verify(
		repository.root,
		MetadataBundle{
			Timestamp: files["timestamp.json"],
			Snapshot:  files["9.snapshot.json"],
			Targets:   files["9.targets.json"],
		},
		TrustedState{RootVersion: 1},
		Installed{
			Version: "v1.1.0", Commit: strings.Repeat("0", 40),
			OS: "windows", Arch: "amd64", APIVersion: "v1", SchemaVersion: 12,
		},
		Policy{
			AllowedTargetKeyIDs: []string{repository.signers["targets"].id},
			Repository:          "ActionsCommunity/multirunner",
			Workflow:            ".github/workflows/release-sign.yml",
			BuilderID:           "https://github.com/actions/runner",
		},
		repository.now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyArtifact(
		bytes.NewReader(files[consistentPathForTarget(t, prepared, verified.TargetPath)]),
		verified.Target,
	); err != nil {
		t.Fatal(err)
	}
}

func TestPreparedRepositoryRejectsTamperingAndIncompletePlatforms(t *testing.T) {
	repository := newTestRepository(t)
	tests := map[string]func(*PreparedRepository){
		"extra target": func(prepared *PreparedRepository) {
			prepared.TargetFiles["targets/"+strings.Repeat("0", 64)+".extra"] = []byte("extra")
		},
		"hash mismatch": func(prepared *PreparedRepository) {
			prepared.TargetFiles[prepared.Manifest.Files[0].Path][0] ^= 1
		},
		"provenance subject mismatch": func(prepared *PreparedRepository) {
			for _, file := range prepared.Manifest.Files {
				if file.Kind == "provenance" {
					prepared.TargetFiles[file.Path] = bytes.Replace(
						prepared.TargetFiles[file.Path],
						[]byte(`"name":"`), []byte(`"name":"wrong-`), 1,
					)
					return
				}
			}
		},
		"wrong source run": func(prepared *PreparedRepository) {
			prepared.Manifest.SourceRunID++
		},
		"noncanonical payload": func(prepared *PreparedRepository) {
			var value any
			if err := json.Unmarshal(prepared.TargetsPayload, &value); err != nil {
				t.Fatal(err)
			}
			prepared.TargetsPayload, _ = json.MarshalIndent(value, "", "  ")
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			prepared := prepareTestRepository(t, repository)
			mutate(&prepared)
			if err := VerifyPreparedRepository(repository.root, prepared); err == nil {
				t.Fatal("tampered signing request was accepted")
			}
		})
	}

	options := testPrepareOptions(t, repository)
	options.Artifacts = options.Artifacts[:len(options.Artifacts)-1]
	if _, err := PrepareRepository(options); err == nil {
		t.Fatal("missing release platform was accepted")
	}
}

func TestPreparedRepositoryRejectsUnsupportedRoleThreshold(t *testing.T) {
	repository := newTestRepository(t)
	root := decodeRoot(t, repository.root)
	root.Signed.Roles["targets"] = Role{
		KeyIDs:    []string{repository.signers["targets"].id, repository.signers["root"].id},
		Threshold: 2,
	}
	repository.root = signEnvelope(t, root.Signed, repository.signers["root"])
	options := testPrepareOptions(t, repository)
	options.Root = repository.root
	if _, err := PrepareRepository(options); !errors.Is(err, ErrUntrusted) {
		t.Fatalf("threshold error = %v", err)
	}
}

func TestPrepareRepositoryRejectsArtifactIdentityMismatches(t *testing.T) {
	repository := newTestRepository(t)
	tests := map[string]func(*PrepareOptions){
		"binary revision": func(options *PrepareOptions) {
			options.Artifacts[0].Revision = strings.Repeat("2", 40)
		},
		"repository": func(options *PrepareOptions) {
			mutateProvenance(t, &options.Artifacts[0], func(statement map[string]any) {
				dependencies := statement["predicate"].(map[string]any)["buildDefinition"].(map[string]any)["resolvedDependencies"].([]any)
				dependencies[0].(map[string]any)["uri"] = "git+https://github.com/attacker/repo"
			})
		},
		"commit": func(options *PrepareOptions) {
			mutateProvenance(t, &options.Artifacts[0], func(statement map[string]any) {
				dependencies := statement["predicate"].(map[string]any)["buildDefinition"].(map[string]any)["resolvedDependencies"].([]any)
				dependencies[0].(map[string]any)["digest"].(map[string]any)["gitCommit"] =
					strings.Repeat("2", 40)
			})
		},
		"builder": func(options *PrepareOptions) {
			mutateProvenance(t, &options.Artifacts[0], func(statement map[string]any) {
				statement["predicate"].(map[string]any)["runDetails"].(map[string]any)["builder"].(map[string]any)["id"] = "attacker"
			})
		},
		"invocation": func(options *PrepareOptions) {
			mutateProvenance(t, &options.Artifacts[0], func(statement map[string]any) {
				statement["predicate"].(map[string]any)["runDetails"].(map[string]any)["metadata"].(map[string]any)["invocationId"] = "attacker"
			})
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			options := testPrepareOptions(t, repository)
			mutate(&options)
			if _, err := PrepareRepository(options); err == nil {
				t.Fatal("artifact identity mismatch was accepted")
			}
		})
	}
}

func TestFinalRepositoryRejectsLinkageAndMutation(t *testing.T) {
	repository := newTestRepository(t)
	tests := map[string]func(map[string][]byte){
		"mutation after signing": func(files map[string][]byte) {
			for name := range files {
				if strings.HasPrefix(name, "targets/") {
					files[name][0] ^= 1
					return
				}
			}
		},
		"timestamp linked to wrong snapshot": func(files map[string][]byte) {
			var timestamp Envelope[Timestamp]
			mustJSON(t, files["timestamp.json"], &timestamp)
			meta := timestamp.Signed.Meta["snapshot.json"]
			meta.Hashes["sha256"] = strings.Repeat("0", 64)
			timestamp.Signed.Meta["snapshot.json"] = meta
			files["timestamp.json"] = signEnvelope(t, timestamp.Signed, repository.signers["timestamp"])
		},
		"snapshot linked to unsigned targets": func(files map[string][]byte) {
			var targets Envelope[Targets]
			mustJSON(t, files["9.targets.json"], &targets)
			unsigned, err := canonicalJSON(targets.Signed)
			if err != nil {
				t.Fatal(err)
			}
			var snapshot Envelope[Snapshot]
			mustJSON(t, files["9.snapshot.json"], &snapshot)
			snapshot.Signed.Meta["targets.json"] = linkedMeta(9, unsigned)
			files["9.snapshot.json"] = signEnvelope(t, snapshot.Signed, repository.signers["snapshot"])
			var timestamp Envelope[Timestamp]
			mustJSON(t, files["timestamp.json"], &timestamp)
			timestamp.Signed.Meta["snapshot.json"] = linkedMeta(9, files["9.snapshot.json"])
			files["timestamp.json"] = signEnvelope(t, timestamp.Signed, repository.signers["timestamp"])
		},
		"targets signed with wrong role key": func(files map[string][]byte) {
			var targets Envelope[Targets]
			mustJSON(t, files["9.targets.json"], &targets)
			files["9.targets.json"] = signEnvelope(
				t, targets.Signed, repository.signers["snapshot"],
			)
			var snapshot Envelope[Snapshot]
			mustJSON(t, files["9.snapshot.json"], &snapshot)
			snapshot.Signed.Meta["targets.json"] = linkedMeta(9, files["9.targets.json"])
			files["9.snapshot.json"] = signEnvelope(t, snapshot.Signed, repository.signers["snapshot"])
			var timestamp Envelope[Timestamp]
			mustJSON(t, files["timestamp.json"], &timestamp)
			timestamp.Signed.Meta["snapshot.json"] = linkedMeta(9, files["9.snapshot.json"])
			files["timestamp.json"] = signEnvelope(t, timestamp.Signed, repository.signers["timestamp"])
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			prepared := prepareTestRepository(t, repository)
			files := signPreparedRepository(t, repository, prepared)
			mutate(files)
			if err := VerifyFinalRepositoryAgainstPrepared(repository.root, prepared, files); err == nil {
				t.Fatal("invalid final repository was accepted")
			}
		})
	}
}

func mutateProvenance(
	t *testing.T,
	artifact *ReleaseArtifact,
	mutate func(map[string]any),
) {
	t.Helper()
	var statement map[string]any
	if err := json.Unmarshal(artifact.Provenance, &statement); err != nil {
		t.Fatal(err)
	}
	mutate(statement)
	data, err := json.Marshal(statement)
	if err != nil {
		t.Fatal(err)
	}
	artifact.Provenance = data
}

func TestCanonicalSigningPayloadMatchesJQCompactSortedOutput(t *testing.T) {
	vectors := []string{
		`{"z":2,"a":1}`,
		`{"unicode":"café 東京","escaped":"line\nquote\"slash\\"}`,
		`{"nested":{"b":[true,null,"x"],"a":-7}}`,
	}
	for _, vector := range vectors {
		got, err := CanonicalizeSigningPayload([]byte(vector))
		if err != nil {
			t.Fatal(err)
		}
		var value any
		if err := json.Unmarshal([]byte(vector), &value); err != nil {
			t.Fatal(err)
		}
		want, err := canonicalJSON(value)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("canonical output = %q, want %q", got, want)
		}
	}
	jq, err := exec.LookPath("jq")
	if err != nil {
		t.Log("jq is unavailable; static canonical vectors remain verified")
		return
	}
	for _, vector := range vectors {
		command := exec.Command(jq, "-cSj", ".")
		command.Stdin = strings.NewReader(vector)
		jqOutput, err := command.Output()
		if err != nil {
			t.Fatal(err)
		}
		goOutput, err := CanonicalizeSigningPayload([]byte(vector))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(jqOutput, goOutput) {
			t.Fatalf("jq = %q, Go = %q", jqOutput, goOutput)
		}
	}
}

func prepareTestRepository(t *testing.T, repository *testRepository) PreparedRepository {
	t.Helper()
	prepared, err := PrepareRepository(testPrepareOptions(t, repository))
	if err != nil {
		t.Fatal(err)
	}
	return prepared
}

func testPrepareOptions(t *testing.T, repository *testRepository) PrepareOptions {
	t.Helper()
	const version = "v1.2.0"
	commit := strings.Repeat("1", 40)
	invocation := "https://github.com/ActionsCommunity/multirunner/actions/runs/42/attempts/1"
	options := PrepareOptions{
		Root: repository.root, MetadataVersion: 9,
		Version: version, Commit: commit, APIVersion: "v1", SchemaMin: 1, SchemaMax: 12,
		EmbeddedConsoleSHA256: strings.Repeat("c", 64),
		Repository:            "ActionsCommunity/multirunner",
		Workflow:              ".github/workflows/release-sign.yml",
		BuilderID:             "https://github.com/actions/runner",
		InvocationID:          invocation, SourceRunID: 42, Now: repository.now,
		LegalFiles: map[string][]byte{
			"LICENSE":                []byte("project license\n"),
			"THIRD_PARTY_NOTICES.md": []byte("third-party notices\n"),
		},
	}
	for _, platform := range releasePlatforms {
		name := fmt.Sprintf("multirunner_%s_%s_%s", version, platform.OS, platform.Arch)
		if platform.OS == "windows" {
			name += ".exe"
		}
		executable := []byte("trusted executable " + platform.OS + "/" + platform.Arch)
		digest := sha256.Sum256(executable)
		sbom, err := json.Marshal(map[string]any{
			"bomFormat": "CycloneDX",
			"metadata": map[string]any{
				"component": map[string]any{
					"name": name, "version": version,
					"hashes": []map[string]string{{
						"alg": "SHA-256", "content": hex.EncodeToString(digest[:]),
					}},
				},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		provenance, err := json.Marshal(map[string]any{
			"_type": "https://in-toto.io/Statement/v1",
			"subject": []map[string]any{{
				"name": name, "digest": map[string]string{"sha256": hex.EncodeToString(digest[:])},
			}},
			"predicateType": "https://slsa.dev/provenance/v1",
			"predicate": map[string]any{
				"buildDefinition": map[string]any{
					"externalParameters": map[string]string{
						"version": version, "os": platform.OS, "arch": platform.Arch,
					},
					"resolvedDependencies": []map[string]any{{
						"uri":    "git+https://github.com/ActionsCommunity/multirunner",
						"digest": map[string]string{"gitCommit": commit},
					}},
				},
				"runDetails": map[string]any{
					"builder":  map[string]string{"id": "https://github.com/actions/runner"},
					"metadata": map[string]string{"invocationId": invocation},
				},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		options.Artifacts = append(options.Artifacts, ReleaseArtifact{
			Name: name, OS: platform.OS, Arch: platform.Arch, Revision: commit,
			Executable: executable, SBOM: sbom, Provenance: provenance,
		})
	}
	return options
}

func signPreparedRepository(
	t *testing.T,
	repository *testRepository,
	prepared PreparedRepository,
) map[string][]byte {
	t.Helper()
	var targets Targets
	if err := json.Unmarshal(prepared.TargetsPayload, &targets); err != nil {
		t.Fatal(err)
	}
	targetsEnvelope := signEnvelope(t, targets, repository.signers["targets"])
	snapshot := signEnvelope(t, Snapshot{
		Type: "snapshot", SpecVersion: SpecVersion, Version: prepared.Manifest.MetadataVersion,
		Expires: prepared.Manifest.SnapshotExpires,
		Meta: map[string]FileMeta{
			"targets.json": linkedMeta(prepared.Manifest.MetadataVersion, targetsEnvelope),
		},
	}, repository.signers["snapshot"])
	timestamp := signEnvelope(t, Timestamp{
		Type: "timestamp", SpecVersion: SpecVersion, Version: prepared.Manifest.MetadataVersion,
		Expires: prepared.Manifest.TimestampExpires,
		Meta: map[string]FileMeta{
			"snapshot.json": linkedMeta(prepared.Manifest.MetadataVersion, snapshot),
		},
	}, repository.signers["timestamp"])
	files := map[string][]byte{
		"timestamp.json": timestamp,
		fmt.Sprintf("%d.snapshot.json", prepared.Manifest.MetadataVersion): snapshot,
		fmt.Sprintf("%d.targets.json", prepared.Manifest.MetadataVersion):  targetsEnvelope,
		"1.root.json": prepared.Root,
	}
	for name, data := range prepared.TargetFiles {
		files[name] = append([]byte(nil), data...)
	}
	return files
}

func consistentPathForTarget(
	t *testing.T,
	prepared PreparedRepository,
	targetPath string,
) string {
	t.Helper()
	for _, file := range prepared.Manifest.Files {
		if file.TargetPath == targetPath {
			return file.Path
		}
	}
	t.Fatalf("target %s is absent from manifest", targetPath)
	return ""
}

func TestCanonicalSigningPayloadRejectsDuplicateKeys(t *testing.T) {
	_, err := CanonicalizeSigningPayload([]byte(`{"a":1,"a":2}`))
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("duplicate key error = %v", err)
	}
}

func TestRepositoryExpiryDurationsAreDeterministic(t *testing.T) {
	repository := newTestRepository(t)
	prepared := prepareTestRepository(t, repository)
	if prepared.Manifest.TargetsExpires.Sub(prepared.Manifest.GeneratedAt) != 365*24*time.Hour ||
		prepared.Manifest.SnapshotExpires.Sub(prepared.Manifest.GeneratedAt) != 180*24*time.Hour ||
		prepared.Manifest.TimestampExpires.Sub(prepared.Manifest.GeneratedAt) != 90*24*time.Hour {
		t.Fatalf("unexpected expiries: %+v", prepared.Manifest)
	}
}

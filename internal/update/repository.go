package update

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

const SigningRequestSchema = "multirunner.update-signing-request/v1"

var releasePlatforms = []struct {
	OS   string
	Arch string
}{
	{"linux", "amd64"},
	{"linux", "arm64"},
	{"windows", "amd64"},
	{"windows", "arm64"},
	{"darwin", "amd64"},
	{"darwin", "arm64"},
}

type ReleaseArtifact struct {
	Name       string
	OS         string
	Arch       string
	Revision   string
	Executable []byte
	SBOM       []byte
	Provenance []byte
}

type PrepareOptions struct {
	Root                  []byte
	MetadataVersion       int
	Version               string
	Commit                string
	APIVersion            string
	SchemaMin             int
	SchemaMax             int
	EmbeddedConsoleSHA256 string
	Repository            string
	Workflow              string
	BuilderID             string
	InvocationID          string
	SourceRunID           int64
	Now                   time.Time
	Artifacts             []ReleaseArtifact
	LegalFiles            map[string][]byte
}

type SigningManifest struct {
	Schema                string        `json:"schema"`
	Repository            string        `json:"repository"`
	ReleaseTag            string        `json:"release_tag"`
	ReleaseCommit         string        `json:"release_commit"`
	SourceRunID           int64         `json:"source_run_id"`
	MetadataVersion       int           `json:"metadata_version"`
	APIVersion            string        `json:"api_version"`
	SchemaMin             int           `json:"schema_min"`
	SchemaMax             int           `json:"schema_max"`
	EmbeddedConsoleSHA256 string        `json:"embedded_console_sha256"`
	Workflow              string        `json:"workflow"`
	BuilderID             string        `json:"builder_id"`
	InvocationID          string        `json:"invocation_id"`
	GeneratedAt           time.Time     `json:"generated_at"`
	TargetsExpires        time.Time     `json:"targets_expires"`
	SnapshotExpires       time.Time     `json:"snapshot_expires"`
	TimestampExpires      time.Time     `json:"timestamp_expires"`
	RootSHA256            string        `json:"root_sha256"`
	TargetsPayloadSHA256  string        `json:"targets_payload_sha256"`
	Files                 []SigningFile `json:"files"`
}

type SigningFile struct {
	Path       string `json:"path"`
	TargetPath string `json:"target_path"`
	Length     int64  `json:"length"`
	SHA256     string `json:"sha256"`
	OS         string `json:"os,omitempty"`
	Arch       string `json:"arch,omitempty"`
	Kind       string `json:"kind"`
}

type PreparedRepository struct {
	Root           []byte
	TargetsPayload []byte
	TargetFiles    map[string][]byte
	Evidence       map[string][]byte
	Manifest       SigningManifest
}

func PrepareRepository(options PrepareOptions) (PreparedRepository, error) {
	if options.MetadataVersion < 1 || !validReleaseVersion(options.Version) ||
		!validCommit(options.Commit) || options.APIVersion == "" ||
		options.SchemaMin < 1 || options.SchemaMax < options.SchemaMin ||
		!validSHA256(options.EmbeddedConsoleSHA256) ||
		options.Repository == "" || options.Workflow == "" || options.BuilderID == "" ||
		options.InvocationID == "" || options.SourceRunID < 1 {
		return PreparedRepository{}, errors.New("release repository options are incomplete")
	}
	if options.Now.IsZero() {
		options.Now = time.Now().UTC()
	}
	options.Now = options.Now.UTC().Truncate(time.Second)
	root, err := validateSigningRoot(options.Root, options.Now)
	if err != nil {
		return PreparedRepository{}, err
	}

	targetFiles := map[string][]byte{}
	targetMetadata := map[string]TargetFile{}
	manifestFiles := make([]SigningFile, 0, len(options.Artifacts)*3+2)
	platforms := map[string]struct{}{}
	buildEvidence := make([]map[string]string, 0, len(options.Artifacts))
	for _, artifact := range options.Artifacts {
		if err := validateReleaseArtifact(options, artifact); err != nil {
			return PreparedRepository{}, err
		}
		platform := artifact.OS + "/" + artifact.Arch
		if _, exists := platforms[platform]; exists {
			return PreparedRepository{}, fmt.Errorf("duplicate release artifact platform %s", platform)
		}
		platforms[platform] = struct{}{}

		executableTarget := path.Join("targets", artifact.Name)
		executableEntry := addPreparedFile(
			targetFiles, executableTarget, artifact.Executable,
			artifact.OS, artifact.Arch, "executable",
		)
		executableHash := sha256.Sum256(artifact.Executable)
		sbomHash := sha256.Sum256(artifact.SBOM)
		provenanceHash := sha256.Sum256(artifact.Provenance)
		targetMetadata[executableTarget] = TargetFile{
			Length: int64(len(artifact.Executable)),
			Hashes: map[string]string{"sha256": hex.EncodeToString(executableHash[:])},
			Custom: TargetMetadata{
				Version: options.Version, Commit: options.Commit,
				OS: artifact.OS, Arch: artifact.Arch, APIVersion: options.APIVersion,
				SchemaMin: options.SchemaMin, SchemaMax: options.SchemaMax,
				EmbeddedConsoleSHA256: options.EmbeddedConsoleSHA256,
				SBOMSHA256:            hex.EncodeToString(sbomHash[:]),
				ProvenanceSHA256:      hex.EncodeToString(provenanceHash[:]),
				Provenance: Provenance{
					Repository: options.Repository, Commit: options.Commit,
					Workflow: options.Workflow, BuilderID: options.BuilderID,
				},
			},
		}
		manifestFiles = append(manifestFiles, executableEntry)
		for _, sidecar := range []struct {
			suffix string
			kind   string
			data   []byte
		}{
			{".cdx.json", "sbom", artifact.SBOM},
			{".provenance.json", "provenance", artifact.Provenance},
		} {
			targetName := executableTarget + sidecar.suffix
			entry := addPreparedFile(
				targetFiles, targetName, sidecar.data,
				artifact.OS, artifact.Arch, sidecar.kind,
			)
			digest := sha256.Sum256(sidecar.data)
			targetMetadata[targetName] = TargetFile{
				Length: int64(len(sidecar.data)),
				Hashes: map[string]string{"sha256": hex.EncodeToString(digest[:])},
			}
			manifestFiles = append(manifestFiles, entry)
		}
		buildEvidence = append(buildEvidence, map[string]string{
			"name": artifact.Name, "os": artifact.OS, "arch": artifact.Arch,
			"vcs_revision": strings.ToLower(artifact.Revision),
		})
	}
	if err := requireReleasePlatforms(platforms); err != nil {
		return PreparedRepository{}, err
	}
	for _, name := range []string{"LICENSE", "THIRD_PARTY_NOTICES.md"} {
		data, ok := options.LegalFiles[name]
		if !ok || len(data) == 0 {
			return PreparedRepository{}, fmt.Errorf("release legal file %s is missing", name)
		}
		targetName := path.Join("targets", name)
		entry := addPreparedFile(targetFiles, targetName, data, "", "", "license")
		digest := sha256.Sum256(data)
		targetMetadata[targetName] = TargetFile{
			Length: int64(len(data)),
			Hashes: map[string]string{"sha256": hex.EncodeToString(digest[:])},
		}
		manifestFiles = append(manifestFiles, entry)
	}
	if len(options.LegalFiles) != 2 {
		return PreparedRepository{}, errors.New("release legal file set is not exact")
	}
	sort.Slice(manifestFiles, func(i, j int) bool { return manifestFiles[i].Path < manifestFiles[j].Path })
	sort.Slice(buildEvidence, func(i, j int) bool { return buildEvidence[i]["name"] < buildEvidence[j]["name"] })

	targetsExpires := options.Now.Add(365 * 24 * time.Hour)
	targetsPayload, err := canonicalJSON(Targets{
		Type: "targets", SpecVersion: SpecVersion, Version: options.MetadataVersion,
		Expires: targetsExpires, Targets: targetMetadata,
	})
	if err != nil {
		return PreparedRepository{}, fmt.Errorf("canonicalize targets payload: %w", err)
	}
	rootDigest := sha256.Sum256(options.Root)
	payloadDigest := sha256.Sum256(targetsPayload)
	buildInfo, err := json.MarshalIndent(map[string]any{
		"schema":         "multirunner.update-build-info/v1",
		"release_tag":    options.Version,
		"release_commit": strings.ToLower(options.Commit),
		"source_run_id":  options.SourceRunID,
		"artifacts":      buildEvidence,
	}, "", "  ")
	if err != nil {
		return PreparedRepository{}, err
	}
	prepared := PreparedRepository{
		Root:           append([]byte(nil), options.Root...),
		TargetsPayload: targetsPayload,
		TargetFiles:    targetFiles,
		Evidence: map[string][]byte{
			"console-tree.sha256": []byte(options.EmbeddedConsoleSHA256 + "\n"),
			"build-info.json":     buildInfo,
		},
		Manifest: SigningManifest{
			Schema: SigningRequestSchema, Repository: options.Repository,
			ReleaseTag: options.Version, ReleaseCommit: strings.ToLower(options.Commit),
			SourceRunID: options.SourceRunID, MetadataVersion: options.MetadataVersion,
			APIVersion: options.APIVersion, SchemaMin: options.SchemaMin, SchemaMax: options.SchemaMax,
			EmbeddedConsoleSHA256: options.EmbeddedConsoleSHA256,
			Workflow:              options.Workflow, BuilderID: options.BuilderID, InvocationID: options.InvocationID,
			GeneratedAt: options.Now, TargetsExpires: targetsExpires,
			SnapshotExpires:      options.Now.Add(180 * 24 * time.Hour),
			TimestampExpires:     options.Now.Add(90 * 24 * time.Hour),
			RootSHA256:           hex.EncodeToString(rootDigest[:]),
			TargetsPayloadSHA256: hex.EncodeToString(payloadDigest[:]),
			Files:                manifestFiles,
		},
	}
	if err := verifyPreparedRepository(root, prepared, options.Now); err != nil {
		return PreparedRepository{}, err
	}
	return prepared, nil
}

func VerifyPreparedRepository(root []byte, prepared PreparedRepository) error {
	envelope, err := validateSigningRoot(root, prepared.Manifest.GeneratedAt)
	if err != nil {
		return err
	}
	return verifyPreparedRepository(envelope, prepared, prepared.Manifest.GeneratedAt)
}

func VerifyFinalRepository(root []byte, files map[string][]byte) error {
	envelope, err := validateSigningRoot(root, time.Now().UTC())
	if err != nil {
		return err
	}
	return verifyFinalRepository(envelope, files)
}

func VerifyFinalRepositoryAgainstPrepared(
	root []byte,
	prepared PreparedRepository,
	files map[string][]byte,
) error {
	envelope, err := validateSigningRoot(root, prepared.Manifest.GeneratedAt)
	if err != nil {
		return err
	}
	if err := verifyPreparedRepository(envelope, prepared, prepared.Manifest.GeneratedAt); err != nil {
		return err
	}
	if err := verifyFinalRepository(envelope, files); err != nil {
		return err
	}
	var timestampEnvelope Envelope[Timestamp]
	if err := decodeStrictEnvelope(files["timestamp.json"], &timestampEnvelope); err != nil {
		return err
	}
	snapshotName := fmt.Sprintf("%d.snapshot.json", prepared.Manifest.MetadataVersion)
	var snapshotEnvelope Envelope[Snapshot]
	if err := decodeStrictEnvelope(files[snapshotName], &snapshotEnvelope); err != nil {
		return err
	}
	targetsName := fmt.Sprintf("%d.targets.json", prepared.Manifest.MetadataVersion)
	var targetsEnvelope Envelope[Targets]
	if err := decodeStrictEnvelope(files[targetsName], &targetsEnvelope); err != nil {
		return fmt.Errorf("decode final targets: %w", err)
	}
	if timestampEnvelope.Signed.Version != prepared.Manifest.MetadataVersion ||
		snapshotEnvelope.Signed.Version != prepared.Manifest.MetadataVersion ||
		targetsEnvelope.Signed.Version != prepared.Manifest.MetadataVersion ||
		!timestampEnvelope.Signed.Expires.Equal(prepared.Manifest.TimestampExpires) ||
		!snapshotEnvelope.Signed.Expires.Equal(prepared.Manifest.SnapshotExpires) ||
		!targetsEnvelope.Signed.Expires.Equal(prepared.Manifest.TargetsExpires) {
		return fmt.Errorf("%w: final metadata identity differs from authorized request", ErrIntegrity)
	}
	actualPayload, err := canonicalJSON(targetsEnvelope.Signed)
	if err != nil {
		return err
	}
	if !bytes.Equal(actualPayload, prepared.TargetsPayload) {
		return fmt.Errorf("%w: signed targets payload differs from authorized request", ErrIntegrity)
	}
	for name, expected := range prepared.TargetFiles {
		actual, ok := files[name]
		if !ok || !bytes.Equal(actual, expected) {
			return fmt.Errorf("%w: final target %s differs from authorized request", ErrIntegrity, name)
		}
	}
	return nil
}

func CanonicalizeSigningPayload(data []byte) ([]byte, error) {
	if err := rejectDuplicateKeys(data); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("%w: invalid signing payload: %v", ErrIntegrity, err)
	}
	return canonicalJSON(value)
}

func validateSigningRoot(data []byte, now time.Time) (Envelope[Root], error) {
	if now.IsZero() {
		return Envelope[Root]{}, errors.New("signing validation time is required")
	}
	root, err := decodeEnvelope[Root](data)
	if err != nil {
		return Envelope[Root]{}, err
	}
	if err := validateRoot(root.Signed, now); err != nil {
		return Envelope[Root]{}, err
	}
	if !root.Signed.ConsistentSnapshot {
		return Envelope[Root]{}, fmt.Errorf("%w: root disables consistent snapshots", ErrIntegrity)
	}
	if err := verifyRole(root, "root", root.Signatures, root.Signed, Policy{}); err != nil {
		return Envelope[Root]{}, fmt.Errorf("verify trusted root: %w", err)
	}
	for _, roleName := range []string{"timestamp", "snapshot", "targets"} {
		role := root.Signed.Roles[roleName]
		if role.Threshold != 1 {
			return Envelope[Root]{}, fmt.Errorf(
				"%w: %s threshold %d is unsupported by the isolated signer",
				ErrUntrusted, roleName, role.Threshold,
			)
		}
	}
	return root, nil
}

func verifyPreparedRepository(root Envelope[Root], prepared PreparedRepository, now time.Time) error {
	manifest := prepared.Manifest
	if manifest.Schema != SigningRequestSchema ||
		manifest.MetadataVersion < 1 || manifest.SourceRunID < 1 ||
		!validReleaseVersion(manifest.ReleaseTag) || !validCommit(manifest.ReleaseCommit) ||
		manifest.Repository == "" || manifest.Workflow == "" || manifest.BuilderID == "" ||
		manifest.InvocationID == "" || manifest.APIVersion == "" ||
		!strings.Contains(manifest.InvocationID, "/runs/"+strconv.FormatInt(manifest.SourceRunID, 10)+"/") ||
		manifest.SchemaMin < 1 || manifest.SchemaMax < manifest.SchemaMin ||
		!validSHA256(manifest.EmbeddedConsoleSHA256) ||
		!manifest.GeneratedAt.Equal(now.UTC().Truncate(time.Second)) ||
		!manifest.TargetsExpires.After(now) ||
		!manifest.SnapshotExpires.After(now) ||
		!manifest.TimestampExpires.After(now) {
		return errors.New("signing request manifest is incomplete")
	}
	var supplied Envelope[Root]
	if err := decodeStrictEnvelope(prepared.Root, &supplied); err != nil {
		return err
	}
	authorizedCanonical, err := canonicalJSON(root)
	if err != nil {
		return err
	}
	suppliedCanonical, err := canonicalJSON(supplied)
	if err != nil || !bytes.Equal(authorizedCanonical, suppliedCanonical) {
		return fmt.Errorf("%w: request root differs from authorized root", ErrIntegrity)
	}
	rootDigest := sha256.Sum256(prepared.Root)
	if !strings.EqualFold(manifest.RootSHA256, hex.EncodeToString(rootDigest[:])) {
		return fmt.Errorf("%w: request root digest mismatch", ErrIntegrity)
	}
	if err := rejectDuplicateKeys(prepared.TargetsPayload); err != nil {
		return err
	}
	var targets Targets
	if err := decodeStrictJSON(prepared.TargetsPayload, &targets); err != nil {
		return fmt.Errorf("decode targets payload: %w", err)
	}
	canonical, err := canonicalJSON(targets)
	if err != nil {
		return err
	}
	if !bytes.Equal(canonical, prepared.TargetsPayload) {
		return fmt.Errorf("%w: targets payload is not canonical", ErrIntegrity)
	}
	payloadDigest := sha256.Sum256(prepared.TargetsPayload)
	if !strings.EqualFold(manifest.TargetsPayloadSHA256, hex.EncodeToString(payloadDigest[:])) {
		return fmt.Errorf("%w: targets payload digest mismatch", ErrIntegrity)
	}
	if targets.Type != "targets" || targets.SpecVersion != SpecVersion ||
		targets.Version != manifest.MetadataVersion ||
		!targets.Expires.Equal(manifest.TargetsExpires) {
		return fmt.Errorf("%w: targets payload identity mismatch", ErrIntegrity)
	}
	if string(prepared.Evidence["console-tree.sha256"]) != manifest.EmbeddedConsoleSHA256+"\n" {
		return fmt.Errorf("%w: console tree evidence mismatch", ErrIntegrity)
	}
	var buildInfo struct {
		Schema        string `json:"schema"`
		ReleaseTag    string `json:"release_tag"`
		ReleaseCommit string `json:"release_commit"`
		SourceRunID   int64  `json:"source_run_id"`
		Artifacts     []struct {
			Name        string `json:"name"`
			OS          string `json:"os"`
			Arch        string `json:"arch"`
			VCSRevision string `json:"vcs_revision"`
		} `json:"artifacts"`
	}
	if err := decodeStrictJSON(prepared.Evidence["build-info.json"], &buildInfo); err != nil ||
		buildInfo.Schema != "multirunner.update-build-info/v1" ||
		buildInfo.ReleaseTag != manifest.ReleaseTag ||
		!strings.EqualFold(buildInfo.ReleaseCommit, manifest.ReleaseCommit) ||
		buildInfo.SourceRunID != manifest.SourceRunID {
		return fmt.Errorf("%w: build info evidence mismatch", ErrIntegrity)
	}
	buildByPlatform := map[string]string{}
	for _, artifact := range buildInfo.Artifacts {
		platform := artifact.OS + "/" + artifact.Arch
		if _, duplicate := buildByPlatform[platform]; duplicate ||
			!strings.EqualFold(artifact.VCSRevision, manifest.ReleaseCommit) {
			return fmt.Errorf("%w: invalid build info evidence for %s", ErrIntegrity, platform)
		}
		buildByPlatform[platform] = artifact.Name
	}
	if err := requireReleasePlatforms(stringMapSet(buildByPlatform)); err != nil {
		return err
	}

	manifestByPath := map[string]SigningFile{}
	platforms := map[string]struct{}{}
	for _, file := range manifest.Files {
		if err := validRepositoryPath(file.Path); err != nil ||
			errIfInvalidTargetPath(file.TargetPath) != nil ||
			file.Length < 1 || !validSHA256(file.SHA256) {
			return fmt.Errorf("%w: invalid manifest file %q", ErrIntegrity, file.Path)
		}
		if _, duplicate := manifestByPath[file.Path]; duplicate {
			return fmt.Errorf("%w: duplicate manifest file %q", ErrIntegrity, file.Path)
		}
		manifestByPath[file.Path] = file
		data, ok := prepared.TargetFiles[file.Path]
		if !ok || int64(len(data)) != file.Length || !hashMatches(data, file.SHA256) {
			return fmt.Errorf("%w: manifest file %s does not match request data", ErrIntegrity, file.Path)
		}
		target, ok := targets.Targets[file.TargetPath]
		if !ok || target.Length != file.Length || !strings.EqualFold(target.Hashes["sha256"], file.SHA256) ||
			file.Path != consistentTargetPath(file.TargetPath, sha256.Sum256(data)) {
			return fmt.Errorf("%w: target metadata does not describe %s", ErrIntegrity, file.Path)
		}
		switch file.Kind {
		case "executable":
			if file.OS == "" || file.Arch == "" {
				return fmt.Errorf("%w: executable platform missing", ErrIntegrity)
			}
			platform := file.OS + "/" + file.Arch
			if _, duplicate := platforms[platform]; duplicate {
				return fmt.Errorf("%w: duplicate executable platform %s", ErrIntegrity, platform)
			}
			platforms[platform] = struct{}{}
			if buildByPlatform[platform] != path.Base(file.TargetPath) {
				return fmt.Errorf("%w: executable build evidence mismatch for %s", ErrIntegrity, platform)
			}
			if err := validateExecutableTarget(manifest, file, target, prepared); err != nil {
				return err
			}
		case "sbom", "provenance":
			if file.OS == "" || file.Arch == "" {
				return fmt.Errorf("%w: sidecar platform missing", ErrIntegrity)
			}
		case "license":
			if file.OS != "" || file.Arch != "" ||
				(file.TargetPath != "targets/LICENSE" &&
					file.TargetPath != "targets/THIRD_PARTY_NOTICES.md") {
				return fmt.Errorf("%w: invalid legal target %s", ErrIntegrity, file.TargetPath)
			}
		default:
			return fmt.Errorf("%w: unsupported signing file kind %q", ErrIntegrity, file.Kind)
		}
	}
	if len(manifestByPath) != len(prepared.TargetFiles) ||
		len(targets.Targets) != len(prepared.TargetFiles) {
		return fmt.Errorf("%w: undeclared or missing target files", ErrIntegrity)
	}
	return requireReleasePlatforms(platforms)
}

func verifyFinalRepository(root Envelope[Root], files map[string][]byte) error {
	if len(files) == 0 {
		return errors.New("final repository is empty")
	}
	timestamp, ok := files["timestamp.json"]
	if !ok {
		return fmt.Errorf("%w: final repository omits timestamp.json", ErrIntegrity)
	}
	var timestampEnvelope Envelope[Timestamp]
	if err := decodeStrictEnvelope(timestamp, &timestampEnvelope); err != nil {
		return err
	}
	if err := validateMetadata(
		"timestamp", timestampEnvelope.Signed.Type, timestampEnvelope.Signed.SpecVersion,
		timestampEnvelope.Signed.Version, 0, timestampEnvelope.Signed.Expires, time.Now().UTC(),
	); err != nil {
		return err
	}
	if err := verifyRole(root, "timestamp", timestampEnvelope.Signatures, timestampEnvelope.Signed, Policy{}); err != nil {
		return err
	}
	snapshotMeta, ok := timestampEnvelope.Signed.Meta["snapshot.json"]
	if !ok || len(timestampEnvelope.Signed.Meta) != 1 {
		return fmt.Errorf("%w: timestamp metadata links unexpected files", ErrIntegrity)
	}
	snapshotName := fmt.Sprintf("%d.snapshot.json", snapshotMeta.Version)
	snapshot, ok := files[snapshotName]
	if !ok {
		return fmt.Errorf("%w: final repository omits %s", ErrIntegrity, snapshotName)
	}
	if err := verifyLinked("snapshot.json", snapshot, snapshotMeta); err != nil {
		return err
	}
	var snapshotEnvelope Envelope[Snapshot]
	if err := decodeStrictEnvelope(snapshot, &snapshotEnvelope); err != nil {
		return err
	}
	if snapshotEnvelope.Signed.Version != snapshotMeta.Version {
		return fmt.Errorf("%w: snapshot version does not match timestamp", ErrIntegrity)
	}
	if err := validateMetadata(
		"snapshot", snapshotEnvelope.Signed.Type, snapshotEnvelope.Signed.SpecVersion,
		snapshotEnvelope.Signed.Version, 0, snapshotEnvelope.Signed.Expires, time.Now().UTC(),
	); err != nil {
		return err
	}
	if err := verifyRole(root, "snapshot", snapshotEnvelope.Signatures, snapshotEnvelope.Signed, Policy{}); err != nil {
		return err
	}
	targetsMeta, ok := snapshotEnvelope.Signed.Meta["targets.json"]
	if !ok || len(snapshotEnvelope.Signed.Meta) != 1 {
		return fmt.Errorf("%w: snapshot metadata links unexpected files", ErrIntegrity)
	}
	targetsName := fmt.Sprintf("%d.targets.json", targetsMeta.Version)
	targets, ok := files[targetsName]
	if !ok {
		return fmt.Errorf("%w: final repository omits %s", ErrIntegrity, targetsName)
	}
	if err := verifyLinked("targets.json", targets, targetsMeta); err != nil {
		return err
	}
	var targetsEnvelope Envelope[Targets]
	if err := decodeStrictEnvelope(targets, &targetsEnvelope); err != nil {
		return err
	}
	if targetsEnvelope.Signed.Version != targetsMeta.Version {
		return fmt.Errorf("%w: targets version does not match snapshot", ErrIntegrity)
	}
	if err := validateMetadata(
		"targets", targetsEnvelope.Signed.Type, targetsEnvelope.Signed.SpecVersion,
		targetsEnvelope.Signed.Version, 0, targetsEnvelope.Signed.Expires, time.Now().UTC(),
	); err != nil {
		return err
	}
	if err := verifyRole(root, "targets", targetsEnvelope.Signatures, targetsEnvelope.Signed, Policy{}); err != nil {
		return err
	}

	expected := map[string]struct{}{
		"timestamp.json": {}, snapshotName: {}, targetsName: {},
		fmt.Sprintf("%d.root.json", root.Signed.Version): {},
	}
	rootFile := files[fmt.Sprintf("%d.root.json", root.Signed.Version)]
	var finalRoot Envelope[Root]
	if err := decodeStrictEnvelope(rootFile, &finalRoot); err != nil {
		return err
	}
	authorizedCanonical, err := canonicalJSON(root)
	if err != nil {
		return err
	}
	finalCanonical, err := canonicalJSON(finalRoot)
	if err != nil || !bytes.Equal(authorizedCanonical, finalCanonical) {
		return fmt.Errorf("%w: final repository root mismatch", ErrIntegrity)
	}
	for targetPath, target := range targetsEnvelope.Signed.Targets {
		digest, err := decodeSHA256(target.Hashes["sha256"])
		if err != nil || target.Length < 1 {
			return fmt.Errorf("%w: invalid target %s", ErrIntegrity, targetPath)
		}
		consistent := consistentTargetPath(targetPath, digest)
		data, ok := files[consistent]
		if !ok || int64(len(data)) != target.Length || !hashMatches(data, target.Hashes["sha256"]) {
			return fmt.Errorf("%w: target %s is missing or changed", ErrIntegrity, targetPath)
		}
		expected[consistent] = struct{}{}
	}
	if len(files) != len(expected) {
		return fmt.Errorf("%w: final repository contains undeclared files", ErrIntegrity)
	}
	return nil
}

func validateReleaseArtifact(options PrepareOptions, artifact ReleaseArtifact) error {
	if err := validRepositoryPath("targets/" + artifact.Name); err != nil ||
		artifact.OS == "" || artifact.Arch == "" ||
		len(artifact.Executable) == 0 || len(artifact.SBOM) == 0 ||
		len(artifact.Provenance) == 0 ||
		!strings.EqualFold(artifact.Revision, options.Commit) {
		return fmt.Errorf("release artifact %q is incomplete", artifact.Name)
	}
	expected := fmt.Sprintf("multirunner_%s_%s_%s", options.Version, artifact.OS, artifact.Arch)
	if artifact.OS == "windows" {
		expected += ".exe"
	}
	if artifact.Name != expected {
		return fmt.Errorf("release artifact %q has unexpected name, want %q", artifact.Name, expected)
	}
	digest := sha256.Sum256(artifact.Executable)
	if err := validateSBOM(artifact.SBOM, artifact.Name, options.Version, digest); err != nil {
		return fmt.Errorf("%s SBOM: %w", artifact.Name, err)
	}
	if err := validateProvenance(artifact.Provenance, options, artifact, digest); err != nil {
		return fmt.Errorf("%s provenance: %w", artifact.Name, err)
	}
	return nil
}

func validateExecutableTarget(
	manifest SigningManifest,
	file SigningFile,
	target TargetFile,
	prepared PreparedRepository,
) error {
	custom := target.Custom
	if custom.Version != manifest.ReleaseTag || !strings.EqualFold(custom.Commit, manifest.ReleaseCommit) ||
		custom.OS != file.OS || custom.Arch != file.Arch ||
		custom.APIVersion != manifest.APIVersion ||
		custom.SchemaMin != manifest.SchemaMin || custom.SchemaMax != manifest.SchemaMax ||
		custom.EmbeddedConsoleSHA256 != manifest.EmbeddedConsoleSHA256 ||
		custom.Provenance.Repository != manifest.Repository ||
		!strings.EqualFold(custom.Provenance.Commit, manifest.ReleaseCommit) ||
		custom.Provenance.Workflow != manifest.Workflow ||
		custom.Provenance.BuilderID != manifest.BuilderID {
		return fmt.Errorf("%w: executable target %s identity mismatch", ErrIntegrity, file.TargetPath)
	}
	sbomTarget := file.TargetPath + ".cdx.json"
	provenanceTarget := file.TargetPath + ".provenance.json"
	sbom, ok := findPreparedTarget(prepared, sbomTarget)
	if !ok || !hashMatches(sbom, custom.SBOMSHA256) {
		return fmt.Errorf("%w: executable target %s SBOM mismatch", ErrIntegrity, file.TargetPath)
	}
	provenance, ok := findPreparedTarget(prepared, provenanceTarget)
	if !ok || !hashMatches(provenance, custom.ProvenanceSHA256) {
		return fmt.Errorf("%w: executable target %s provenance mismatch", ErrIntegrity, file.TargetPath)
	}
	executable := prepared.TargetFiles[file.Path]
	digest := sha256.Sum256(executable)
	if err := validateSBOM(sbom, path.Base(file.TargetPath), manifest.ReleaseTag, digest); err != nil {
		return err
	}
	options := PrepareOptions{
		Version: manifest.ReleaseTag, Commit: manifest.ReleaseCommit,
		Repository: manifest.Repository, Workflow: manifest.Workflow,
		BuilderID: manifest.BuilderID, InvocationID: manifest.InvocationID,
	}
	return validateProvenance(provenance, options, ReleaseArtifact{
		Name: path.Base(file.TargetPath), OS: file.OS, Arch: file.Arch,
	}, digest)
}

func validateSBOM(data []byte, name, version string, digest [sha256.Size]byte) error {
	var document struct {
		BOMFormat string `json:"bomFormat"`
		Metadata  struct {
			Component struct {
				Name    string `json:"name"`
				Version string `json:"version"`
				Hashes  []struct {
					Algorithm string `json:"alg"`
					Content   string `json:"content"`
				} `json:"hashes"`
			} `json:"component"`
		} `json:"metadata"`
	}
	if err := decodeStrictJSON(data, &document); err != nil {
		return err
	}
	if document.BOMFormat != "CycloneDX" ||
		document.Metadata.Component.Name != name ||
		document.Metadata.Component.Version != version {
		return errors.New("identity mismatch")
	}
	expected := hex.EncodeToString(digest[:])
	for _, hash := range document.Metadata.Component.Hashes {
		if hash.Algorithm == "SHA-256" && strings.EqualFold(hash.Content, expected) {
			return nil
		}
	}
	return errors.New("executable digest mismatch")
}

func validateProvenance(
	data []byte,
	options PrepareOptions,
	artifact ReleaseArtifact,
	digest [sha256.Size]byte,
) error {
	var statement struct {
		Type    string `json:"_type"`
		Subject []struct {
			Name   string            `json:"name"`
			Digest map[string]string `json:"digest"`
		} `json:"subject"`
		PredicateType string `json:"predicateType"`
		Predicate     struct {
			BuildDefinition struct {
				ExternalParameters   map[string]string `json:"externalParameters"`
				ResolvedDependencies []struct {
					URI    string            `json:"uri"`
					Digest map[string]string `json:"digest"`
				} `json:"resolvedDependencies"`
			} `json:"buildDefinition"`
			RunDetails struct {
				Builder  map[string]string `json:"builder"`
				Metadata map[string]string `json:"metadata"`
			} `json:"runDetails"`
		} `json:"predicate"`
	}
	if err := decodeStrictJSON(data, &statement); err != nil {
		return err
	}
	expectedDigest := hex.EncodeToString(digest[:])
	if statement.Type != "https://in-toto.io/Statement/v1" ||
		statement.PredicateType != "https://slsa.dev/provenance/v1" ||
		len(statement.Subject) != 1 ||
		statement.Subject[0].Name != artifact.Name ||
		!strings.EqualFold(statement.Subject[0].Digest["sha256"], expectedDigest) ||
		statement.Predicate.BuildDefinition.ExternalParameters["version"] != options.Version ||
		statement.Predicate.BuildDefinition.ExternalParameters["os"] != artifact.OS ||
		statement.Predicate.BuildDefinition.ExternalParameters["arch"] != artifact.Arch ||
		len(statement.Predicate.BuildDefinition.ResolvedDependencies) != 1 ||
		statement.Predicate.BuildDefinition.ResolvedDependencies[0].URI != "git+https://github.com/"+options.Repository ||
		!strings.EqualFold(
			statement.Predicate.BuildDefinition.ResolvedDependencies[0].Digest["gitCommit"],
			options.Commit,
		) ||
		statement.Predicate.RunDetails.Builder["id"] != options.BuilderID ||
		statement.Predicate.RunDetails.Metadata["invocationId"] != options.InvocationID {
		return errors.New("identity or subject mismatch")
	}
	return nil
}

func addPreparedFile(
	files map[string][]byte,
	targetPath string,
	data []byte,
	goos, goarch, kind string,
) SigningFile {
	digest := sha256.Sum256(data)
	physicalPath := consistentTargetPath(targetPath, digest)
	files[physicalPath] = append([]byte(nil), data...)
	return SigningFile{
		Path: physicalPath, TargetPath: targetPath,
		Length: int64(len(data)), SHA256: hex.EncodeToString(digest[:]),
		OS: goos, Arch: goarch, Kind: kind,
	}
}

func findPreparedTarget(prepared PreparedRepository, targetPath string) ([]byte, bool) {
	for _, file := range prepared.Manifest.Files {
		if file.TargetPath == targetPath {
			data, ok := prepared.TargetFiles[file.Path]
			return data, ok
		}
	}
	return nil, false
}

func requireReleasePlatforms(platforms map[string]struct{}) error {
	if len(platforms) != len(releasePlatforms) {
		return fmt.Errorf("%w: release platform set has %d entries, want %d",
			ErrIntegrity, len(platforms), len(releasePlatforms))
	}
	for _, platform := range releasePlatforms {
		name := platform.OS + "/" + platform.Arch
		if _, ok := platforms[name]; !ok {
			return fmt.Errorf("%w: release platform %s is missing", ErrIntegrity, name)
		}
	}
	return nil
}

func stringMapSet(values map[string]string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for key := range values {
		result[key] = struct{}{}
	}
	return result
}

func decodeStrictJSON(data []byte, destination any) error {
	if err := rejectDuplicateKeys(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("%w: %v", ErrIntegrity, err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("%w: trailing JSON data", ErrIntegrity)
	}
	return nil
}

func decodeStrictEnvelope[T any](data []byte, destination *Envelope[T]) error {
	envelope, err := decodeEnvelope[T](data)
	if err != nil {
		return err
	}
	*destination = envelope
	return nil
}

func errIfInvalidTargetPath(name string) error {
	if !strings.HasPrefix(name, "targets/") {
		return errors.New("target path must be under targets")
	}
	return validRepositoryPath(name)
}

func hashMatches(data []byte, expected string) bool {
	digest := sha256.Sum256(data)
	return strings.EqualFold(expected, hex.EncodeToString(digest[:]))
}

func decodeSHA256(value string) ([sha256.Size]byte, error) {
	var digest [sha256.Size]byte
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		return digest, errors.New("invalid sha256")
	}
	copy(digest[:], decoded)
	return digest, nil
}

func consistentTargetPath(name string, digest [sha256.Size]byte) string {
	directory, file := path.Split(name)
	return path.Join(directory, hex.EncodeToString(digest[:])+"."+file)
}

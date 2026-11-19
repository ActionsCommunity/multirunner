package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	buildinfofile "debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/GerardSmit/multirunner/internal/history"
	"github.com/GerardSmit/multirunner/internal/update"
)

var platforms = []struct {
	os   string
	arch string
}{
	{"linux", "amd64"},
	{"linux", "arm64"},
	{"windows", "amd64"},
	{"windows", "arm64"},
	{"darwin", "amd64"},
	{"darwin", "arm64"},
}

type options struct {
	command         string
	dist            string
	request         string
	repositoryDir   string
	output          string
	root            string
	console         string
	version         string
	commit          string
	repository      string
	workflow        string
	builderID       string
	invocationID    string
	metadataVersion int
	sourceRunID     int64
}

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "update-repository:", err)
		os.Exit(1)
	}
}

func run(_ context.Context, args []string) error {
	opts, err := parseArgs(args)
	if err != nil {
		return err
	}
	switch opts.command {
	case "prepare":
		return prepare(opts)
	case "verify-prepared":
		return verifyPrepared(opts)
	case "verify-final":
		return verifyFinal(opts)
	default:
		return fmt.Errorf("unsupported command %q", opts.command)
	}
}

func prepare(opts options) error {
	root, err := os.ReadFile(opts.root)
	if err != nil {
		return fmt.Errorf("read trusted root: %w", err)
	}
	encodedRoot, err := update.EncodeEmbeddedRoot(root)
	if err != nil {
		return fmt.Errorf("validate trusted root: %w", err)
	}
	consoleDigest, err := digestTree(opts.console)
	if err != nil {
		return fmt.Errorf("digest embedded console: %w", err)
	}
	artifacts := make([]update.ReleaseArtifact, 0, len(platforms))
	for _, platform := range platforms {
		extension := ""
		if platform.os == "windows" {
			extension = ".exe"
		}
		name := fmt.Sprintf(
			"multirunner_%s_%s_%s%s",
			opts.version, platform.os, platform.arch, extension,
		)
		location := filepath.Join(opts.dist, name)
		executable, err := os.ReadFile(location)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		if !bytes.Contains(executable, []byte(encodedRoot)) {
			return fmt.Errorf("%s does not embed the signing repository trust root", name)
		}
		build, err := buildinfofile.ReadFile(location)
		if err != nil {
			return fmt.Errorf("read build info for %s: %w", name, err)
		}
		revision, err := verifyReleaseBuildInfo(build, opts.commit)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		digest := sha256.Sum256(executable)
		sbom, err := buildSBOM(name, opts.version, digest, build)
		if err != nil {
			return err
		}
		provenance, err := buildProvenance(opts, name, platform.os, platform.arch, digest)
		if err != nil {
			return err
		}
		artifacts = append(artifacts, update.ReleaseArtifact{
			Name: name, OS: platform.os, Arch: platform.arch, Revision: revision,
			Executable: executable, SBOM: sbom, Provenance: provenance,
		})
	}
	legalFiles := make(map[string][]byte, 2)
	for _, name := range []string{"LICENSE", "THIRD_PARTY_NOTICES.md"} {
		data, err := os.ReadFile(filepath.Join(opts.dist, name))
		if err != nil {
			return fmt.Errorf("read release legal file %s: %w", name, err)
		}
		legalFiles[name] = data
	}
	prepared, err := update.PrepareRepository(update.PrepareOptions{
		Root: root, MetadataVersion: opts.metadataVersion,
		Version: opts.version, Commit: opts.commit,
		APIVersion: "v1", SchemaMin: 1, SchemaMax: history.CurrentSchemaVersion(),
		EmbeddedConsoleSHA256: consoleDigest,
		Repository:            opts.repository, Workflow: opts.workflow,
		BuilderID: opts.builderID, InvocationID: opts.invocationID,
		SourceRunID: opts.sourceRunID, Now: time.Now().UTC(),
		Artifacts: artifacts, LegalFiles: legalFiles,
	})
	if err != nil {
		return err
	}
	return writePreparedRepository(opts.output, prepared)
}

func verifyReleaseBuildInfo(build *buildinfofile.BuildInfo, commit string) (string, error) {
	revision := buildSetting(build, "vcs.revision")
	if !strings.EqualFold(revision, commit) {
		return "", fmt.Errorf("source revision %q does not match %s", revision, commit)
	}
	if modified := buildSetting(build, "vcs.modified"); modified != "false" {
		return "", fmt.Errorf("source modification state is %q, want false", modified)
	}
	return revision, nil
}

func verifyPrepared(opts options) error {
	prepared, err := readPreparedRepository(opts.request)
	if err != nil {
		return err
	}
	if err := verifyExpectedIdentity(opts, prepared.Manifest); err != nil {
		return err
	}
	if err := update.VerifyPreparedRepository(prepared.Root, prepared); err != nil {
		return fmt.Errorf("verify prepared repository: %w", err)
	}
	return nil
}

func verifyFinal(opts options) error {
	prepared, err := readPreparedRepository(opts.request)
	if err != nil {
		return err
	}
	if err := verifyExpectedIdentity(opts, prepared.Manifest); err != nil {
		return err
	}
	files, err := readFileTree(opts.repositoryDir)
	if err != nil {
		return fmt.Errorf("read final repository: %w", err)
	}
	if err := update.VerifyFinalRepositoryAgainstPrepared(prepared.Root, prepared, files); err != nil {
		return fmt.Errorf("verify final repository: %w", err)
	}
	if opts.dist != "" {
		for _, file := range prepared.Manifest.Files {
			if file.Kind != "executable" {
				continue
			}
			name := filepath.Base(file.TargetPath)
			data, err := os.ReadFile(filepath.Join(opts.dist, name))
			if err != nil {
				return fmt.Errorf("read publication binary %s: %w", name, err)
			}
			if int64(len(data)) != file.Length || !digestMatches(data, file.SHA256) {
				return fmt.Errorf("publication binary %s differs from authorized request", name)
			}
		}
	}
	return nil
}

func parseArgs(args []string) (options, error) {
	if len(args) == 0 {
		return options{}, errors.New("command is required: prepare, verify-prepared, or verify-final")
	}
	opts := options{command: args[0]}
	set := flag.NewFlagSet("update-repository "+opts.command, flag.ContinueOnError)
	set.StringVar(&opts.dist, "dist", "", "directory containing release binaries")
	set.StringVar(&opts.request, "request", "", "signing request directory")
	set.StringVar(&opts.repositoryDir, "repository-dir", "", "signed update repository directory")
	set.StringVar(&opts.output, "output", "", "output signing request directory")
	set.StringVar(&opts.root, "root", "", "threshold-signed root metadata")
	set.StringVar(&opts.console, "console", "internal/consoleui/dist", "embedded console directory")
	set.StringVar(&opts.version, "version", "", "release version")
	set.StringVar(&opts.commit, "commit", "", "release source commit")
	set.StringVar(&opts.repository, "repository", "", "source repository identity")
	set.StringVar(&opts.workflow, "workflow", "", "release workflow identity")
	set.StringVar(&opts.builderID, "builder-id", "", "provenance builder identity")
	set.StringVar(&opts.invocationID, "invocation-id", "", "CI invocation identity")
	set.IntVar(&opts.metadataVersion, "metadata-version", 0, "monotonic metadata version")
	set.Int64Var(&opts.sourceRunID, "source-run-id", 0, "authorized source workflow run ID")
	if err := set.Parse(args[1:]); err != nil {
		return options{}, err
	}
	if set.NArg() != 0 {
		return options{}, errors.New("unexpected positional arguments")
	}
	if opts.version == "" || opts.commit == "" || opts.repository == "" ||
		opts.workflow == "" || opts.builderID == "" || opts.invocationID == "" ||
		opts.sourceRunID < 1 {
		return options{}, errors.New(
			"version, commit, repository, workflow, builder-id, invocation-id, and source-run-id are required",
		)
	}
	if strings.ContainsAny(opts.version, `/\`) {
		return options{}, errors.New("version cannot contain path separators")
	}
	switch opts.command {
	case "prepare":
		if opts.root == "" || opts.dist == "" || opts.output == "" || opts.metadataVersion < 1 {
			return options{}, errors.New("prepare requires root, dist, output, and metadata-version")
		}
	case "verify-prepared":
		if opts.request == "" {
			return options{}, errors.New("verify-prepared requires request")
		}
	case "verify-final":
		if opts.request == "" || opts.repositoryDir == "" {
			return options{}, errors.New("verify-final requires request and repository-dir")
		}
	default:
		return options{}, fmt.Errorf("unknown command %q", opts.command)
	}
	return opts, nil
}

func writePreparedRepository(directory string, prepared update.PreparedRepository) error {
	if err := os.Mkdir(directory, 0o755); err != nil {
		return fmt.Errorf("create new output directory: %w", err)
	}
	manifest, err := json.MarshalIndent(prepared.Manifest, "", "  ")
	if err != nil {
		return err
	}
	files := map[string][]byte{
		"request.json":                 append(manifest, '\n'),
		"root.json":                    prepared.Root,
		"payloads/targets.signed.json": prepared.TargetsPayload,
	}
	for name, data := range prepared.TargetFiles {
		files[name] = data
	}
	for name, data := range prepared.Evidence {
		files[filepath.ToSlash(filepath.Join("evidence", name))] = data
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	var sums strings.Builder
	for _, name := range names {
		digest := sha256.Sum256(files[name])
		fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(digest[:]), name)
	}
	files["SHA256SUMS"] = []byte(sums.String())
	for name, data := range files {
		destination := filepath.Join(directory, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(destination, data, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}
	return nil
}

func readPreparedRepository(directory string) (update.PreparedRepository, error) {
	request, err := os.ReadFile(filepath.Join(directory, "request.json"))
	if err != nil {
		return update.PreparedRepository{}, fmt.Errorf("read request manifest: %w", err)
	}
	if _, err := update.CanonicalizeSigningPayload(request); err != nil {
		return update.PreparedRepository{}, fmt.Errorf("validate request manifest JSON: %w", err)
	}
	var manifest update.SigningManifest
	if err := decodeStrictJSON(request, &manifest); err != nil {
		return update.PreparedRepository{}, fmt.Errorf("decode request manifest: %w", err)
	}
	root, err := os.ReadFile(filepath.Join(directory, "root.json"))
	if err != nil {
		return update.PreparedRepository{}, err
	}
	payload, err := os.ReadFile(filepath.Join(directory, "payloads", "targets.signed.json"))
	if err != nil {
		return update.PreparedRepository{}, err
	}
	prepared := update.PreparedRepository{
		Root: root, TargetsPayload: payload,
		TargetFiles: map[string][]byte{}, Evidence: map[string][]byte{},
		Manifest: manifest,
	}
	for _, file := range manifest.Files {
		if !safeRequestPath(file.Path) || !strings.HasPrefix(file.Path, "targets/") {
			return update.PreparedRepository{}, fmt.Errorf("invalid signing request path %q", file.Path)
		}
		data, err := os.ReadFile(filepath.Join(directory, filepath.FromSlash(file.Path)))
		if err != nil {
			return update.PreparedRepository{}, fmt.Errorf("read %s: %w", file.Path, err)
		}
		prepared.TargetFiles[file.Path] = data
	}
	for _, name := range []string{"console-tree.sha256", "build-info.json"} {
		data, err := os.ReadFile(filepath.Join(directory, "evidence", name))
		if err != nil {
			return update.PreparedRepository{}, fmt.Errorf("read evidence %s: %w", name, err)
		}
		prepared.Evidence[name] = data
	}
	expectedFiles := map[string]struct{}{
		"request.json": {}, "root.json": {}, "payloads/targets.signed.json": {},
		"evidence/console-tree.sha256": {}, "evidence/build-info.json": {},
	}
	for _, file := range manifest.Files {
		expectedFiles[file.Path] = struct{}{}
	}
	if err := verifyChecksumFile(directory, expectedFiles); err != nil {
		return update.PreparedRepository{}, err
	}
	return prepared, nil
}

func verifyChecksumFile(directory string, expected map[string]struct{}) error {
	data, err := os.ReadFile(filepath.Join(directory, "SHA256SUMS"))
	if err != nil {
		return fmt.Errorf("read SHA256SUMS: %w", err)
	}
	seen := map[string]struct{}{}
	for lineNumber, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		parts := strings.SplitN(line, "  ", 2)
		if len(parts) != 2 || !validSHA256(parts[0]) ||
			!safeRequestPath(parts[1]) {
			return fmt.Errorf("invalid SHA256SUMS line %d", lineNumber+1)
		}
		if _, duplicate := seen[parts[1]]; duplicate {
			return fmt.Errorf("duplicate SHA256SUMS path %s", parts[1])
		}
		seen[parts[1]] = struct{}{}
		content, err := os.ReadFile(filepath.Join(directory, filepath.FromSlash(parts[1])))
		if err != nil {
			return err
		}
		if !digestMatches(content, parts[0]) {
			return fmt.Errorf("SHA256SUMS mismatch for %s", parts[1])
		}
	}
	if len(seen) != len(expected) {
		return errors.New("SHA256SUMS file set is incomplete")
	}
	for name := range expected {
		if _, ok := seen[name]; !ok {
			return fmt.Errorf("SHA256SUMS omits %s", name)
		}
	}
	actual, err := readFileTree(directory)
	if err != nil {
		return err
	}
	delete(actual, "SHA256SUMS")
	if len(actual) != len(expected) {
		return errors.New("signing request contains undeclared files")
	}
	for name := range actual {
		if _, ok := expected[name]; !ok {
			return fmt.Errorf("signing request contains undeclared file %s", name)
		}
	}
	return nil
}

func safeRequestPath(name string) bool {
	return name != "" && !filepath.IsAbs(name) &&
		!strings.Contains(name, `\`) &&
		path.Clean(name) == name && name != "." &&
		!strings.HasPrefix(name, "../")
}

func readFileTree(root string) (map[string][]byte, error) {
	files := map[string][]byte{}
	err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("symbolic link is forbidden: %s", name)
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("non-regular file is forbidden: %s", name)
		}
		relative, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(relative)], err = os.ReadFile(name)
		return err
	})
	return files, err
}

func verifyExpectedIdentity(opts options, manifest update.SigningManifest) error {
	if manifest.ReleaseTag != opts.version ||
		!strings.EqualFold(manifest.ReleaseCommit, opts.commit) ||
		manifest.Repository != opts.repository ||
		manifest.Workflow != opts.workflow ||
		manifest.BuilderID != opts.builderID ||
		manifest.InvocationID != opts.invocationID ||
		manifest.SourceRunID != opts.sourceRunID {
		return errors.New("signing request does not match authorized release identity")
	}
	return nil
}

func digestTree(root string) (string, error) {
	var names []string
	err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("embedded console contains symbolic link %s", name)
		}
		if !entry.IsDir() {
			relative, err := filepath.Rel(root, name)
			if err != nil {
				return err
			}
			names = append(names, filepath.ToSlash(relative))
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(names) == 0 {
		return "", errors.New("embedded console directory is empty")
	}
	sort.Strings(names)
	hash := sha256.New()
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			return "", err
		}
		_, _ = hash.Write([]byte(name))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(data)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func buildSetting(build *debug.BuildInfo, key string) string {
	for _, setting := range build.Settings {
		if setting.Key == key {
			return setting.Value
		}
	}
	return ""
}

func buildSBOM(
	name, version string,
	digest [sha256.Size]byte,
	build *debug.BuildInfo,
) ([]byte, error) {
	type component struct {
		Type    string              `json:"type"`
		Name    string              `json:"name"`
		Version string              `json:"version,omitempty"`
		PURL    string              `json:"purl,omitempty"`
		Hashes  []map[string]string `json:"hashes,omitempty"`
	}
	components := make([]component, 0, len(build.Deps)+1)
	for _, dependency := range build.Deps {
		module := dependency
		if dependency.Replace != nil {
			module = dependency.Replace
		}
		components = append(components, component{
			Type: "library", Name: module.Path, Version: module.Version,
			PURL: "pkg:golang/" + module.Path + "@" + module.Version,
		})
	}
	sort.Slice(components, func(i, j int) bool {
		return components[i].Name < components[j].Name
	})
	document := map[string]any{
		"bomFormat": "CycloneDX", "specVersion": "1.6", "version": 1,
		"metadata": map[string]any{
			"component": component{
				Type: "application", Name: name, Version: version,
				Hashes: []map[string]string{{"alg": "SHA-256", "content": hex.EncodeToString(digest[:])}},
			},
			"tools": []map[string]any{{
				"vendor": "Go", "name": "toolchain", "version": build.GoVersion,
			}},
		},
		"components": components,
	}
	return json.MarshalIndent(document, "", "  ")
}

func buildProvenance(
	opts options,
	name, goos, goarch string,
	digest [sha256.Size]byte,
) ([]byte, error) {
	statement := map[string]any{
		"_type": "https://in-toto.io/Statement/v1",
		"subject": []map[string]any{{
			"name":   name,
			"digest": map[string]string{"sha256": hex.EncodeToString(digest[:])},
		}},
		"predicateType": "https://slsa.dev/provenance/v1",
		"predicate": map[string]any{
			"buildDefinition": map[string]any{
				"buildType": "https://github.com/GerardSmit/multirunner/build/v1",
				"externalParameters": map[string]string{
					"version": opts.version, "os": goos, "arch": goarch,
				},
				"resolvedDependencies": []map[string]any{{
					"uri":    "git+https://github.com/" + opts.repository,
					"digest": map[string]string{"gitCommit": opts.commit},
				}},
			},
			"runDetails": map[string]any{
				"builder":  map[string]string{"id": opts.builderID},
				"metadata": map[string]string{"invocationId": opts.invocationID},
			},
		},
	}
	return json.MarshalIndent(statement, "", "  ")
}

func decodeStrictJSON(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}

func digestMatches(data []byte, expected string) bool {
	digest := sha256.Sum256(data)
	return strings.EqualFold(hex.EncodeToString(digest[:]), expected)
}

func validSHA256(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

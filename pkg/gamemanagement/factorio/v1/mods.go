package v1

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"
)

const (
	ModProviderID            = "factorio-mod-portal"
	SupportedFactorioVersion = "2.0"
	SupportedRuntimeVersion  = "2.0.77"
	// RuntimeImage is the Plexus-owned Factorio supervisor image. The tag is
	// the seed dedicated-server patch shipped in the image; boot updates to
	// the selected channel's latest build.
	RuntimeImage             = "ghcr.io/anthonyposchen/plexus-factorio:" + SupportedRuntimeVersion
	ModApplyPolicyNextStart  = "next-start"
	ModClientSyncJoinTime    = "join-time"
	ModArtifactDataKey       = "archive.zip"
	ModProviderAnnotation    = "plexus.gg/mod-provider-id"
	ModIDAnnotation          = "plexus.gg/mod-id"
	ModVersionAnnotation     = "plexus.gg/mod-version"
	ModSHA256Annotation      = "plexus.gg/mod-sha256"
	MaximumModArchiveBytes   = 900_000
	MaximumModExpandedBytes  = 64 << 20
	MaximumModArchiveEntries = 4096
)

var (
	modIDPattern      = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,100}$`)
	modVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
)

type ModRelease struct {
	ProviderID    string
	ProviderModID string
	Name          string
	Version       string
	GameVersion   string
	Dependencies  []string
}

// ModUpdateCustomerMessage is the Factorio adapter policy for a detected
// provider update. Updates are applied on the next Start or customer Restart
// and never restart a Server automatically.
func ModUpdateCustomerMessage(runtimeRunning bool) string {
	if runtimeRunning {
		return "Restart to apply"
	}
	return "Start to apply"
}

// ValidateModRelease accepts the deliberately narrow first tracer: one
// Factorio 2.0 portal mod whose only required dependency is the base game.
func ValidateModRelease(release ModRelease) error {
	if release.ProviderID != ModProviderID || modIDPattern.MatchString(release.ProviderModID) == false || release.Name != release.ProviderModID {
		return fmt.Errorf("mod provider identity is invalid")
	}
	if modVersionPattern.MatchString(release.Version) == false {
		return fmt.Errorf("mod version %q is invalid", release.Version)
	}
	if release.GameVersion != SupportedFactorioVersion {
		return fmt.Errorf("mod targets Factorio %q; supported version is %q", release.GameVersion, SupportedFactorioVersion)
	}
	for _, dependency := range release.Dependencies {
		parsed, err := parseDependency(dependency)
		if err != nil {
			return err
		}
		if parsed.incompatible {
			return fmt.Errorf("mod declares incompatible dependency %q", parsed.name)
		}
		if parsed.required && parsed.name != "base" {
			return fmt.Errorf("required dependency %q is not supported by the one-mod tracer", parsed.name)
		}
		if parsed.name == "base" && parsed.operator != "" {
			compatible, err := dependencyVersionMatches(SupportedRuntimeVersion, parsed.operator, parsed.version)
			if err != nil {
				return fmt.Errorf("base dependency %q is invalid: %w", dependency, err)
			}
			if compatible == false {
				return fmt.Errorf("base dependency %q is incompatible with Factorio %s", dependency, SupportedRuntimeVersion)
			}
		}
	}
	return nil
}

// ValidateModArchive validates integrity and the adapter-owned Factorio mod
// layout before an artifact may be staged or installed.
func ValidateModArchive(release ModRelease, archive []byte, expectedSHA256 string) error {
	if err := ValidateModRelease(release); err != nil {
		return err
	}
	if len(archive) == 0 || len(archive) > MaximumModArchiveBytes {
		return fmt.Errorf("mod archive size must be between 1 and %d bytes", MaximumModArchiveBytes)
	}
	digest := sha256.Sum256(archive)
	if hex.EncodeToString(digest[:]) != strings.ToLower(strings.TrimSpace(expectedSHA256)) {
		return fmt.Errorf("mod archive SHA-256 does not match provider metadata")
	}
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return fmt.Errorf("open mod archive: %w", err)
	}
	if len(reader.File) == 0 || len(reader.File) > MaximumModArchiveEntries {
		return fmt.Errorf("mod archive has an invalid entry count")
	}
	wantRoot := release.Name + "_" + release.Version
	var expanded int64
	var infoJSON []byte
	seen := map[string]struct{}{}
	for _, entry := range reader.File {
		clean, err := safeModArchivePath(entry.Name)
		if err != nil {
			return err
		}
		if _, duplicate := seen[clean]; duplicate {
			return fmt.Errorf("mod archive contains duplicate entry %q", clean)
		}
		seen[clean] = struct{}{}
		if strings.Split(clean, "/")[0] != wantRoot {
			return fmt.Errorf("mod archive entry %q is outside expected root %q", clean, wantRoot)
		}
		if entry.Mode()&0o170000 != 0 && entry.Mode().IsDir() == false && entry.Mode().IsRegular() == false {
			return fmt.Errorf("mod archive entry %q is not a regular file", clean)
		}
		if entry.FileInfo().IsDir() {
			continue
		}
		entryReader, err := entry.Open()
		if err != nil {
			return fmt.Errorf("open mod archive entry %q: %w", clean, err)
		}
		capture := clean == wantRoot+"/info.json"
		content, count, copyErr := streamModArchiveEntry(entryReader, int64(MaximumModExpandedBytes)-expanded+1, capture)
		closeErr := entryReader.Close()
		expanded += count
		if expanded > MaximumModExpandedBytes {
			return fmt.Errorf("mod archive expands beyond %d bytes", MaximumModExpandedBytes)
		}
		if copyErr != nil {
			return fmt.Errorf("read mod archive entry %q: %w", clean, copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close mod archive entry %q: %w", clean, closeErr)
		}
		if capture {
			infoJSON = content
		}
	}
	if infoJSON == nil {
		return fmt.Errorf("mod archive is missing %s/info.json", wantRoot)
	}
	return validateModInfo(infoJSON, release)
}

func streamModArchiveEntry(reader io.Reader, maximum int64, capture bool) ([]byte, int64, error) {
	limited := &io.LimitedReader{R: reader, N: maximum}
	var output io.Writer = io.Discard
	var content bytes.Buffer
	if capture {
		output = &content
	}
	written, err := io.Copy(output, limited)
	if err != nil {
		return nil, written, err
	}
	return content.Bytes(), written, nil
}

func safeModArchivePath(name string) (string, error) {
	if name == "" || strings.Contains(name, "\\") || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("mod archive contains unsafe path %q", name)
	}
	clean := path.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean != strings.TrimSuffix(name, "/") {
		return "", fmt.Errorf("mod archive contains unsafe path %q", name)
	}
	return clean, nil
}

func validateModInfo(content []byte, release ModRelease) error {
	if len(content) > 64<<10 {
		return fmt.Errorf("mod info.json is too large")
	}
	var info struct {
		Name         string   `json:"name"`
		Version      string   `json:"version"`
		Factorio     string   `json:"factorio_version"`
		Dependencies []string `json:"dependencies"`
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	if err := decoder.Decode(&info); err != nil {
		return fmt.Errorf("decode mod info.json: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return fmt.Errorf("decode mod info.json: trailing data")
	}
	if info.Name != release.Name || info.Version != release.Version || info.Factorio != release.GameVersion {
		return fmt.Errorf("mod archive identity does not match provider metadata")
	}
	if strings.Join(info.Dependencies, "\x00") != strings.Join(release.Dependencies, "\x00") {
		return fmt.Errorf("mod archive dependencies do not match provider metadata")
	}
	return nil
}

type modDependency struct {
	name, operator, version string
	required, incompatible  bool
}

func parseDependency(value string) (modDependency, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return modDependency{}, fmt.Errorf("mod declares an empty dependency")
	}
	dependency := modDependency{required: true}
	for _, prefix := range []string{"(?)", "?", "+", "~", "!"} {
		if strings.HasPrefix(value, prefix) {
			value = strings.TrimSpace(strings.TrimPrefix(value, prefix))
			dependency.required = prefix == "+"
			dependency.incompatible = prefix == "!"
			break
		}
	}
	fields := strings.Fields(value)
	if len(fields) != 1 && len(fields) != 3 {
		return modDependency{}, fmt.Errorf("mod dependency %q has invalid syntax", value)
	}
	dependency.name = fields[0]
	if modIDPattern.MatchString(dependency.name) == false {
		return modDependency{}, fmt.Errorf("mod dependency %q is invalid", value)
	}
	if len(fields) == 3 {
		if dependency.incompatible {
			return modDependency{}, fmt.Errorf("incompatible dependency %q cannot constrain a version", value)
		}
		if fields[1] != "<" && fields[1] != "<=" && fields[1] != "=" && fields[1] != ">=" && fields[1] != ">" {
			return modDependency{}, fmt.Errorf("mod dependency %q uses an invalid operator", value)
		}
		if _, err := parseFactorioVersion(fields[2]); err != nil {
			return modDependency{}, fmt.Errorf("mod dependency %q has an invalid version", value)
		}
		dependency.operator, dependency.version = fields[1], fields[2]
	}
	return dependency, nil
}

func dependencyVersionMatches(runtimeVersion string, operator string, wantedVersion string) (bool, error) {
	runtime, err := parseFactorioVersion(runtimeVersion)
	if err != nil {
		return false, err
	}
	wanted, err := parseFactorioVersion(wantedVersion)
	if err != nil {
		return false, err
	}
	comparison := 0
	for index := range runtime {
		if runtime[index] < wanted[index] {
			comparison = -1
			break
		}
		if runtime[index] > wanted[index] {
			comparison = 1
			break
		}
	}
	switch operator {
	case "<":
		return comparison < 0, nil
	case "<=":
		return comparison <= 0, nil
	case "=":
		return comparison == 0, nil
	case ">=":
		return comparison >= 0, nil
	case ">":
		return comparison > 0, nil
	default:
		return false, fmt.Errorf("unsupported version operator %q", operator)
	}
}

func parseFactorioVersion(value string) ([3]int, error) {
	var version [3]int
	parts := strings.Split(value, ".")
	if len(parts) < 2 || len(parts) > len(version) {
		return version, fmt.Errorf("invalid Factorio version %q", value)
	}
	for index, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return version, fmt.Errorf("invalid Factorio version %q", value)
		}
		parsed, err := strconv.Atoi(part)
		if err != nil || parsed < 0 {
			return version, fmt.Errorf("invalid Factorio version %q", value)
		}
		version[index] = parsed
	}
	return version, nil
}

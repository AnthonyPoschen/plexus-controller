package v1

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
)

func TestFactorioModUpdatePolicyAppliesOnNextStartNeverAutomatically(t *testing.T) {
	policy := Schema().Mods
	if policy.ApplyPolicy != ModApplyPolicyNextStart || policy.AutomaticRestart || policy.ClientSynchronization != ModClientSyncJoinTime || policy.VersionSelection != "latest-compatible" {
		t.Fatalf("Factorio update policy drifted: %#v", policy)
	}
	if ModUpdateCustomerMessage(true) != "Restart to apply" || ModUpdateCustomerMessage(false) != "Start to apply" {
		t.Fatal("Factorio update messaging must stay policy-derived and restart-free")
	}
	if Schema().Broadcast.AutomaticRestart || Schema().Mods.AutomaticRestart {
		t.Fatal("Factorio broadcast and mod apply must never restart automatically")
	}
}

func TestValidateModArchiveAcceptsMatchingProviderRelease(t *testing.T) {
	release := testModRelease()
	archive := testModArchive(t, release, map[string]string{"control.lua": "-- safe fixture"})
	digest := sha256.Sum256(archive)
	if err := ValidateModArchive(release, archive, hex.EncodeToString(digest[:])); err != nil {
		t.Fatal(err)
	}
}

func TestValidateModReleaseRejectsUnsupportedRequiredDependency(t *testing.T) {
	release := testModRelease()
	release.Dependencies = []string{"base >= 2.0", "stdlib >= 1.0"}
	if err := ValidateModRelease(release); err == nil {
		t.Fatal("expected external required dependency to be rejected")
	}
}

func TestValidateModReleaseRejectsEmptyPrefixedDependency(t *testing.T) {
	release := testModRelease()
	release.Dependencies = []string{"?"}
	if err := ValidateModRelease(release); err == nil {
		t.Fatal("expected an empty optional dependency to be rejected")
	}
}

func TestValidateModReleaseEvaluatesBaseVersionConstraints(t *testing.T) {
	for _, test := range []struct {
		dependency string
		valid      bool
	}{
		{dependency: "base >= 2.0", valid: true},
		{dependency: "base <= 2.0.77", valid: true},
		{dependency: "base = 2.0.77", valid: true},
		{dependency: "base > 2.0.77", valid: false},
		{dependency: "base < 2.0", valid: false},
		{dependency: "base => 2.0", valid: false},
		{dependency: "base >= nope", valid: false},
		{dependency: "base >= 2.0 trailing", valid: false},
		{dependency: "! base >= 2.0", valid: false},
	} {
		t.Run(test.dependency, func(t *testing.T) {
			release := testModRelease()
			release.Dependencies = []string{test.dependency}
			err := ValidateModRelease(release)
			if (err == nil) != test.valid {
				t.Fatalf("ValidateModRelease() error = %v, valid=%t", err, test.valid)
			}
		})
	}
}

func TestValidateModArchiveRejectsTraversalAndMetadataDrift(t *testing.T) {
	release := testModRelease()
	for name, files := range map[string]map[string]string{
		"traversal": {"../payload": "unsafe"},
		"identity":  {"info.json": `{"name":"different","version":"1.2.3","factorio_version":"2.0","dependencies":["base >= 2.0"]}`},
	} {
		t.Run(name, func(t *testing.T) {
			archive := testModArchive(t, release, files)
			digest := sha256.Sum256(archive)
			if err := ValidateModArchive(release, archive, hex.EncodeToString(digest[:])); err == nil {
				t.Fatal("expected invalid archive to be rejected")
			}
		})
	}
}

func TestValidateModArchiveStreamsActualExpandedContent(t *testing.T) {
	release := testModRelease()
	archive := testModArchiveBytes(t, release, map[string][]byte{
		"payload.bin": bytes.Repeat([]byte{0}, MaximumModExpandedBytes+1),
	})
	for offset := 0; offset+46 <= len(archive); offset++ {
		if binary.LittleEndian.Uint32(archive[offset:offset+4]) != 0x02014b50 {
			continue
		}
		nameLength := int(binary.LittleEndian.Uint16(archive[offset+28 : offset+30]))
		if offset+46+nameLength <= len(archive) && strings.HasSuffix(string(archive[offset+46:offset+46+nameLength]), "payload.bin") {
			binary.LittleEndian.PutUint32(archive[offset+24:offset+28], 1)
			break
		}
	}
	digest := sha256.Sum256(archive)
	err := ValidateModArchive(release, archive, hex.EncodeToString(digest[:]))
	if err == nil {
		t.Fatalf("under-reported expansion error = %v", err)
	}
}

func TestValidateModArchiveEnforcesEntryCountAndDeclaredSize(t *testing.T) {
	release := testModRelease()
	t.Run("entry count", func(t *testing.T) {
		files := make(map[string][]byte, MaximumModArchiveEntries)
		for index := 0; index < MaximumModArchiveEntries; index++ {
			files[fmt.Sprintf("entry-%04d", index)] = nil
		}
		archive := testModArchiveBytes(t, release, files)
		digest := sha256.Sum256(archive)
		if err := ValidateModArchive(release, archive, hex.EncodeToString(digest[:])); err == nil {
			t.Fatal("expected excessive entry count to be rejected")
		}
	})
	t.Run("archive bytes", func(t *testing.T) {
		archive := bytes.Repeat([]byte("x"), MaximumModArchiveBytes+1)
		digest := sha256.Sum256(archive)
		if err := ValidateModArchive(release, archive, hex.EncodeToString(digest[:])); err == nil {
			t.Fatal("expected oversized archive to be rejected")
		}
	})
}

func testModRelease() ModRelease {
	return ModRelease{ProviderID: ModProviderID, ProviderModID: "tiny-mod", Name: "tiny-mod", Version: "1.2.3", GameVersion: SupportedFactorioVersion, Dependencies: []string{"base >= 2.0"}}
}

func testModArchive(t *testing.T, release ModRelease, additional map[string]string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	root := release.Name + "_" + release.Version + "/"
	info := `{"name":"` + release.Name + `","version":"` + release.Version + `","factorio_version":"` + release.GameVersion + `","dependencies":["base >= 2.0"]}`
	files := map[string]string{root + "info.json": info}
	for name, content := range additional {
		if name == "info.json" {
			files[root+name] = content
		} else {
			files[root+name] = content
		}
	}
	for name, content := range files {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func testModArchiveBytes(t *testing.T, release ModRelease, additional map[string][]byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	root := release.Name + "_" + release.Version + "/"
	files := map[string][]byte{root + "info.json": []byte(`{"name":"` + release.Name + `","version":"` + release.Version + `","factorio_version":"` + release.GameVersion + `","dependencies":["base >= 2.0"]}`)}
	for name, content := range additional {
		files[root+name] = content
	}
	for name, content := range files {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

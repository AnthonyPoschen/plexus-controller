package v1

import (
	"fmt"
	"path"
	"strings"
)

const (
	MaximumSaveArchiveBytes  int64 = 2_147_483_648
	MaximumSaveExpandedBytes int64 = 8_589_934_592
	MaximumSaveEntries             = 100_000
	levelEntryName                 = "level.dat"
	levelInitEntryName             = "level-init.dat"
)

type ArchiveEntry struct {
	Name              string
	UncompressedBytes int64
	Directory         bool
}

// ValidateSaveArchive validates metadata obtained from a zip reader before an
// uploaded archive is allowed to replace the hosted Factorio save. Required
// entry names are basenames: normal Factorio archives contain them beneath one
// top-level save directory, while archive-root entries remain accepted for
// compatibility.
func ValidateSaveArchive(fileName string, mediaType string, archiveBytes int64, entries []ArchiveEntry) error {
	if strings.EqualFold(path.Ext(fileName), ".zip") == false {
		return fmt.Errorf("Factorio save must use the .zip extension")
	}
	if mediaType != "application/zip" {
		return fmt.Errorf("Factorio save must use the application/zip media type")
	}
	if archiveBytes <= 0 || archiveBytes > MaximumSaveArchiveBytes {
		return fmt.Errorf("Factorio save archive size must be between 1 and %d bytes", MaximumSaveArchiveBytes)
	}
	if len(entries) == 0 || len(entries) > MaximumSaveEntries {
		return fmt.Errorf("Factorio save archive must contain between 1 and %d entries", MaximumSaveEntries)
	}

	required := map[string]bool{levelEntryName: false, levelInitEntryName: false}
	seen := make(map[string]bool, len(entries))
	entryNames := make([]string, 0, len(entries))
	var saveRoot string
	var saveRootSet bool
	var expandedBytes int64
	for _, entry := range entries {
		if unsafeArchivePath(entry.Name) {
			return fmt.Errorf("Factorio save archive contains unsafe entry %q", entry.Name)
		}
		if seen[entry.Name] {
			return fmt.Errorf("Factorio save archive contains duplicate entry %q", entry.Name)
		}
		seen[entry.Name] = true
		entryNames = append(entryNames, entry.Name)
		if entry.UncompressedBytes < 0 || entry.UncompressedBytes > MaximumSaveExpandedBytes-expandedBytes {
			return fmt.Errorf("Factorio save expanded size exceeds %d bytes", MaximumSaveExpandedBytes)
		}
		expandedBytes += entry.UncompressedBytes
		if entry.Directory == false {
			entryBase := path.Base(entry.Name)
			if _, ok := required[entryBase]; ok {
				entryRoot, ok := factorioSaveRoot(entry.Name, entryBase)
				if ok == false {
					return fmt.Errorf("Factorio save archive required entry %q must be at archive root or beneath one top-level save directory", entry.Name)
				}
				if saveRootSet && entryRoot != saveRoot {
					return fmt.Errorf("Factorio save archive required entries must share one root directory")
				}
				saveRoot = entryRoot
				saveRootSet = true
				required[entryBase] = true
			}
		}
	}
	if saveRoot != "" {
		for _, name := range entryNames {
			if name != saveRoot && name != saveRoot+"/" && strings.HasPrefix(name, saveRoot+"/") == false {
				return fmt.Errorf("Factorio save archive entries must share save root %q", saveRoot)
			}
		}
	}
	for name, found := range required {
		if found == false {
			return fmt.Errorf("Factorio save archive is missing required entry %q", name)
		}
	}
	return nil
}

func factorioSaveRoot(name string, basename string) (string, bool) {
	if name == basename {
		return "", true
	}
	root, ok := strings.CutSuffix(name, "/"+basename)
	if ok == false || root == "" || strings.Contains(root, "/") {
		return "", false
	}
	return root, true
}

func unsafeArchivePath(name string) bool {
	if name == "" || strings.Contains(name, "\\") || path.IsAbs(name) || windowsDriveAbsolutePath(name) {
		return true
	}
	components := strings.Split(name, "/")
	for index, component := range components {
		if component == "." || component == ".." || (component == "" && index != len(components)-1) {
			return true
		}
	}
	return false
}

func windowsDriveAbsolutePath(name string) bool {
	if len(name) < 3 || name[1] != ':' || name[2] != '/' {
		return false
	}
	return (name[0] >= 'a' && name[0] <= 'z') || (name[0] >= 'A' && name[0] <= 'Z')
}

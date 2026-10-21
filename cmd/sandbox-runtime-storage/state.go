/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// stateDir is the directory where per-mount state files are stored.
// Each mount gets its own <volumeId>.json file so concurrent mount processes
// never write to the same file, eliminating the need for file locking.
const stateDir = "/run/csi/.state"

// MountRecord describes a single dynamic CSI mount.
type MountRecord struct {
	Driver          string `json:"driver"`
	VolumeId        string `json:"volumeId"`
	PVName          string `json:"pvName"`
	SubPath         string `json:"subPath"`
	MountPath       string `json:"mountPath"`       // original user-defined mount path
	SymlinkPath     string `json:"symlinkPath"`     // /runtime-dynamic-volume + mountPath
	ActualMountPath string `json:"actualMountPath"` // real CSI mount directory
}

// MountState is the persisted state for all mounts.
type MountState struct {
	Mounts []MountRecord `json:"mounts"`
}

// extractPVName parses the pv name from a VolumeId.
// VolumeId is generated as "<pvName>-<randomString(6)>" in GenerateCSINodePublishVolumeRequest.
// If the VolumeId does not follow this convention, the whole string is returned.
func extractPVName(volumeId string) string {
	parts := strings.Split(volumeId, "-")
	if len(parts) > 1 && len(parts[len(parts)-1]) == 6 {
		return strings.Join(parts[:len(parts)-1], "-")
	}
	return volumeId
}

// sanitizeSubPath converts a sub-path into a safe directory name.
// Leading/trailing slashes are stripped, and internal '/' are replaced
// with '-' so that the result is a single-level directory name.
func sanitizeSubPath(subPath string) string {
	if subPath == "" || subPath == "/" {
		return "root"
	}
	s := strings.Trim(subPath, "/")
	s = strings.ReplaceAll(s, "/", "-")
	// Limit length to avoid overly long directory names.
	const maxLen = 64
	if len(s) > maxLen {
		s = s[:maxLen]
	}
	return s
}

// buildActualMountPath constructs the real CSI mount directory.
// Format: /run/csi/mount-root/<hostname>/<pvname>/<sanitized-subpath>
func buildActualMountPath(hostname, pvName, subPath string) string {
	return filepath.Join("/run/csi/mount-root", hostname, pvName, sanitizeSubPath(subPath))
}

// createSymlink creates a symlink from link to target.
// It is idempotent: if the symlink already points to the correct target, it returns nil.
// If link exists as a wrong symlink or an empty directory, it is removed and recreated.
func createSymlink(target, link string) error {
	// Ensure parent directory of link exists
	linkDir := filepath.Dir(link)
	if err := os.MkdirAll(linkDir, 0755); err != nil { // #nosec G301
		return fmt.Errorf("failed to create parent directory %s: %w", linkDir, err)
	}

	// Check if link already exists
	fi, err := os.Lstat(link)
	if err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			existingTarget, err := os.Readlink(link)
			if err != nil {
				return fmt.Errorf("failed to read existing symlink: %w", err)
			}
			if existingTarget == target {
				return nil // Already correct
			}
			// Wrong target, remove and recreate
			if err := os.Remove(link); err != nil {
				return fmt.Errorf("failed to remove wrong symlink: %w", err)
			}
		} else if fi.IsDir() {
			entries, err := os.ReadDir(link)
			if err != nil {
				return fmt.Errorf("failed to read directory entries: %w", err)
			}
			if len(entries) > 0 {
				return fmt.Errorf("link path %s is a non-empty directory", link)
			}
			if err := os.Remove(link); err != nil {
				return fmt.Errorf("failed to remove empty directory: %w", err)
			}
		} else {
			if err := os.Remove(link); err != nil {
				return fmt.Errorf("failed to remove existing file: %w", err)
			}
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("failed to stat link path: %w", err)
	}

	return os.Symlink(target, link)
}

// recordStatePath returns the per-mount state file path.
// Uses VolumeId when available; falls back to sanitized mountPath.
func recordStatePath(record MountRecord) string {
	name := record.VolumeId
	if name == "" {
		name = sanitizeSubPath(record.MountPath)
	}
	return filepath.Join(stateDir, name+".json")
}

func saveMountRecord(record MountRecord) error {
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal record: %w", err)
	}

	path := recordStatePath(record)
	if err := os.MkdirAll(stateDir, 0750); err != nil {
		return fmt.Errorf("failed to create state directory: %w", err)
	}

	// Write to a temporary file and rename for atomicity.
	// This prevents reading a partially-written file if the process is killed mid-write.
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0640); err != nil {
		return fmt.Errorf("failed to write temp state file %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("failed to rename temp state file to %s: %w", path, err)
	}

	fmt.Fprintf(os.Stdout, "[saveMountRecord] wrote state file %s (size=%d bytes):\n%s\n", path, len(data), string(data))
	return nil
}

// rebuildStateFromSymlinks scans the symlink base directories and reconstructs
// mount records for any valid symlinks that point to actual mount directories.
// This compensates for cases where the state file was lost but mounts and
// symlinks are still present (e.g., state save failed but symlink succeeded).
func rebuildStateFromSymlinks() error {
	symlinkBases := []string{"/runtime-dynamic-volume", "/data"}

	state, err := loadState()
	if err != nil {
		if os.IsNotExist(err) {
			state = &MountState{Mounts: []MountRecord{}}
		} else {
			return fmt.Errorf("failed to load state: %w", err)
		}
	}

	existingPaths := make(map[string]bool)
	for _, m := range state.Mounts {
		existingPaths[m.SymlinkPath] = true
	}

	for _, base := range symlinkBases {
		_ = filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil // skip inaccessible paths
			}
			if d.Type()&os.ModeSymlink == 0 {
				return nil
			}

			// Skip if already in state
			if existingPaths[path] {
				return nil
			}

			target, err := os.Readlink(path)
			if err != nil {
				return nil
			}

			// Verify target is under /run/csi/mount-root (actual mount path pattern)
			if !strings.HasPrefix(target, "/run/csi/mount-root/") {
				return nil
			}

			// Derive mountPath from symlink path relative to base
			mountPath, _ := filepath.Rel(base, path)
			if mountPath == "" || mountPath == "." {
				mountPath = "/"
			}

			// Extract pvName and subPath from target path
			// Format: /run/csi/mount-root/<hostname>/<pvname>/<subpath>
			parts := strings.Split(target, "/")
			var pvName, subPath string
			if len(parts) >= 5 {
				pvName = parts[len(parts)-2]
				subPath = parts[len(parts)-1]
			}

			record := MountRecord{
				Driver:          "nfs.csi.k8s.io",
				VolumeId:        "", // Cannot be reliably recovered from symlink alone
				PVName:          pvName,
				SubPath:         subPath,
				MountPath:       filepath.Join("/", mountPath),
				SymlinkPath:     path,
				ActualMountPath: target,
			}
			state.Mounts = append(state.Mounts, record)
			fmt.Fprintf(os.Stderr, "rebuilt mount record from symlink: %s -> %s\n", path, target)
			return nil
		})
	}

	if len(state.Mounts) == 0 {
		return nil
	}

	return saveState(state)
}

func loadState() (*MountState, error) {
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		if os.IsNotExist(err) {
			return &MountState{Mounts: []MountRecord{}}, nil
		}
		return nil, err
	}

	var mounts []MountRecord
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(stateDir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to read state file %s: %v\n", path, err)
			continue
		}
		var record MountRecord
		if err := json.Unmarshal(data, &record); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to unmarshal state file %s (size=%d bytes): %v\n", path, len(data), err)
			fmt.Fprintf(os.Stderr, "[loadState] raw content of %s:\n%s\n", path, string(data))

			// Copy corrupted file to log directory for post-mortem analysis.
			if podName := os.Getenv("POD_NAME"); podName != "" {
				logDir := filepath.Join("/var/log/csi", podName)
				_ = os.MkdirAll(logDir, 0755)
				corruptedPath := filepath.Join(logDir, "corrupted-"+entry.Name())
				if cpErr := os.WriteFile(corruptedPath, data, 0640); cpErr == nil {
					fmt.Fprintf(os.Stderr, "[loadState] copied corrupted state to %s\n", corruptedPath)
				} else {
					fmt.Fprintf(os.Stderr, "[loadState] failed to copy corrupted state to %s: %v\n", corruptedPath, cpErr)
				}
			}
			continue
		}
		mounts = append(mounts, record)
	}
	return &MountState{Mounts: mounts}, nil
}

// saveState writes the full MountState by saving each record to its own file
// and removing files for records that are no longer present.
func saveState(state *MountState) error {
	if err := os.MkdirAll(stateDir, 0750); err != nil {
		return fmt.Errorf("failed to create state directory: %w", err)
	}

	// Save all current records.
	for _, record := range state.Mounts {
		if err := saveMountRecord(record); err != nil {
			return err
		}
	}

	// Clean up files for records that are no longer in state.
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return nil
	}

	currentFiles := make(map[string]bool)
	for _, record := range state.Mounts {
		currentFiles[filepath.Base(recordStatePath(record))] = true
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if !currentFiles[entry.Name()] {
			_ = os.Remove(filepath.Join(stateDir, entry.Name()))
		}
	}
	return nil
}

// clearState removes all state files under the state directory.
// It does not remove the directory itself because it may be a volume mount point.
func clearState() error {
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to read state directory: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if err := os.Remove(filepath.Join(stateDir, entry.Name())); err != nil {
			return fmt.Errorf("failed to remove state file %s: %w", entry.Name(), err)
		}
	}
	return nil
}

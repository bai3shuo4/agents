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

// stateFilePath is the path to the persisted mount state file.
// It is stored in an emptyDir volume so it is scoped to the Pod and does not
// need hostname-based subdirectories for isolation.
const stateFilePath = "/run/csi/.state/mounts.json"

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

func saveMountRecord(record MountRecord) error {
	state, err := loadState()
	if err != nil {
		if os.IsNotExist(err) {
			state = &MountState{Mounts: []MountRecord{}}
		} else {
			return fmt.Errorf("failed to load state: %w", err)
		}
	}

	// Deduplicate by mountPath
	found := false
	for i, m := range state.Mounts {
		if m.MountPath == record.MountPath {
			state.Mounts[i] = record
			found = true
			break
		}
	}
	if !found {
		state.Mounts = append(state.Mounts, record)
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal state: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(stateFilePath), 0750); err != nil {
		return fmt.Errorf("failed to create state directory: %w", err)
	}

	if err := os.WriteFile(stateFilePath, data, 0640); err != nil {
		return fmt.Errorf("failed to write state file: %w", err)
	}
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
	data, err := os.ReadFile(stateFilePath)
	if err != nil {
		return nil, err
	}
	var state MountState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("failed to unmarshal state: %w", err)
	}
	return &state, nil
}

// saveState writes the full MountState to the state file.
func saveState(state *MountState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal state: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(stateFilePath), 0750); err != nil {
		return fmt.Errorf("failed to create state directory: %w", err)
	}
	if err := os.WriteFile(stateFilePath, data, 0640); err != nil {
		return fmt.Errorf("failed to write state file: %w", err)
	}
	return nil
}

// clearState removes the state file.
func clearState() error {
	if err := os.Remove(stateFilePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove state file: %w", err)
	}
	return nil
}

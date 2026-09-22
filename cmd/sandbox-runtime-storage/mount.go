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
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/golang/protobuf/proto"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type mountOptions struct {
	driver string
	config string // base64-encoded proto-serialized NodePublishVolumeRequest
}

func newMountCmd() *cobra.Command {
	opts := &mountOptions{}
	cmd := &cobra.Command{
		Use:   "mount",
		Short: "Mount a CSI volume into the sandbox container and create symlink",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMount(cmd.Context(), opts)
		},
	}
	cmd.Flags().StringVar(&opts.driver, "driver", "", "CSI driver name (required)")
	cmd.Flags().StringVar(&opts.config, "config", "", "base64-encoded NodePublishVolumeRequest proto (required)")
	_ = cmd.MarkFlagRequired("driver")
	_ = cmd.MarkFlagRequired("config")
	return cmd
}

func runMount(ctx context.Context, opts *mountOptions) error {
	// 1. Decode base64 -> proto bytes
	protoBytes, err := base64.StdEncoding.DecodeString(opts.config)
	if err != nil {
		return fmt.Errorf("failed to base64-decode config: %w", err)
	}

	// 2. Unmarshal into NodePublishVolumeRequest
	req := &csi.NodePublishVolumeRequest{}
	if err := proto.Unmarshal(protoBytes, req); err != nil {
		return fmt.Errorf("failed to unmarshal NodePublishVolumeRequest: %w", err)
	}

	// Preserve original mountPath (user-facing path from SandboxClaim)
	originalMountPath := req.TargetPath

	// Dynamic volume symlink is created under a configurable base directory so that
	// it survives container restarts (backed by an emptyDir volume).
	// The base directory can be configured per-PV via volumeAttributes:
	//   spec.csi.volumeAttributes.runtimeSymlinkPath: "/data"
	// If not specified, defaults to /runtime-dynamic-volume for backward compatibility.
	symlinkBase := req.VolumeContext["runtimeSymlinkPath"]
	if symlinkBase == "" {
		symlinkBase = "/runtime-dynamic-volume"
	}
	symlinkPath := filepath.Join(symlinkBase, originalMountPath)

	// 3. Build actual mount path: /run/csi/mount-root/<hostname>/<pvname>/<sanitized-subpath>
	//
	// The sub-path (merged from PV base path + SandboxClaim subPath) is encoded
	// into the directory name so that the same PV can be mounted to different
	// mountPaths with different sub-paths without collision.
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "unknown"
	}

	pvName := extractPVName(req.VolumeId)
	subPath := req.VolumeContext["path"]
	actualMountPath := buildActualMountPath(hostname, pvName, subPath)

	fmt.Fprintf(os.Stdout, "decoded NodePublishVolumeRequest:\n")
	fmt.Fprintf(os.Stdout, "  VolumeId:           %s\n", req.VolumeId)
	fmt.Fprintf(os.Stdout, "  Original TargetPath: %s\n", originalMountPath)
	fmt.Fprintf(os.Stdout, "  Actual MountPath:   %s\n", actualMountPath)
	fmt.Fprintf(os.Stdout, "  Readonly:           %t\n", req.Readonly)
	fmt.Fprintf(os.Stdout, "  PVName:             %s\n", pvName)
	fmt.Fprintf(os.Stdout, "  Hostname:           %s\n", hostname)
	fmt.Fprintf(os.Stdout, "  SubPath:            %s\n", subPath)
	if req.VolumeCapability != nil && req.VolumeCapability.GetMount() != nil {
		fmt.Fprintf(os.Stdout, "  FSType:             %s\n", req.VolumeCapability.GetMount().FsType)
	}
	fmt.Fprintf(os.Stdout, "  SymlinkBase:        %s\n", symlinkBase)
	if len(req.VolumeContext) > 0 {
		fmt.Fprintf(os.Stdout, "  VolumeContext:\n")
		for k, v := range req.VolumeContext {
			fmt.Fprintf(os.Stdout, "    %s = %s\n", k, v)
		}
	}

	// 4. Update TargetPath to the actual mount path for the CSI driver
	req.TargetPath = actualMountPath

	// 5. Ensure actual target mount path exists
	if err := os.MkdirAll(actualMountPath, 0750); err != nil {
		return fmt.Errorf("failed to create actual mount path %s: %w", actualMountPath, err)
	}

	// 6. Resolve CSI driver unix socket
	socketPath, err := resolveCSISocket(opts.driver)
	if err != nil {
		return err
	}

	// 7. Dial the CSI Node plugin over unix socket
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	conn, err := grpc.DialContext(
		dialCtx,
		"unix://"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		return fmt.Errorf("failed to connect to csi driver at %s: %w", socketPath, err)
	}
	defer conn.Close()

	// 8. Call NodePublishVolume
	nodeClient := csi.NewNodeClient(conn)
	callCtx, callCancel := context.WithTimeout(ctx, 30*time.Second)
	defer callCancel()

	resp, err := nodeClient.NodePublishVolume(callCtx, req)
	if err != nil {
		return fmt.Errorf("NodePublishVolume failed: %w", err)
	}
	_ = resp

	fmt.Fprintf(os.Stdout, "CSI mounted %s via driver %s\n", actualMountPath, opts.driver)

	// 9. Create symlink under the configured base directory so it survives container restarts.
	if err := createSymlink(actualMountPath, symlinkPath); err != nil {
		// Rollback: unmount the volume to avoid leaving orphaned mounts on the node.
		fmt.Fprintf(os.Stderr, "failed to create symlink %s -> %s, rolling back mount...\n", symlinkPath, actualMountPath)
		rollbackCtx, rollbackCancel := context.WithTimeout(ctx, 30*time.Second)
		_, rollbackErr := nodeClient.NodeUnpublishVolume(rollbackCtx, &csi.NodeUnpublishVolumeRequest{
			VolumeId:   req.VolumeId,
			TargetPath: actualMountPath,
		})
		rollbackCancel()
		if rollbackErr != nil {
			fmt.Fprintf(os.Stderr, "warning: rollback mount failed: %v\n", rollbackErr)
		} else {
			fmt.Fprintf(os.Stderr, "rollback mount succeeded\n")
		}
		return fmt.Errorf("failed to create symlink %s -> %s: %w", symlinkPath, actualMountPath, err)
	}
	fmt.Fprintf(os.Stdout, "created symlink: %s -> %s\n", symlinkPath, actualMountPath)
	fmt.Fprintf(os.Stdout, "user should access mount via: %s\n", symlinkPath)

	// 10. Persist mount state for recovery after container restart
	record := MountRecord{
		Driver:          opts.driver,
		VolumeId:        req.VolumeId,
		PVName:          pvName,
		SubPath:         subPath,
		MountPath:       originalMountPath,
		SymlinkPath:     symlinkPath,
		ActualMountPath: actualMountPath,
	}
	if err := saveMountRecord(record); err != nil {
		// Rollback: remove symlink and unmount the volume to avoid leaving orphaned mounts.
		fmt.Fprintf(os.Stderr, "failed to save mount record, rolling back mount...\n")
		if rmErr := os.Remove(symlinkPath); rmErr != nil && !os.IsNotExist(rmErr) {
			fmt.Fprintf(os.Stderr, "warning: failed to remove symlink during rollback: %v\n", rmErr)
		}
		rollbackCtx, rollbackCancel := context.WithTimeout(ctx, 30*time.Second)
		_, rollbackErr := nodeClient.NodeUnpublishVolume(rollbackCtx, &csi.NodeUnpublishVolumeRequest{
			VolumeId:   req.VolumeId,
			TargetPath: actualMountPath,
		})
		rollbackCancel()
		if rollbackErr != nil {
			fmt.Fprintf(os.Stderr, "warning: rollback mount failed: %v\n", rollbackErr)
		} else {
			fmt.Fprintf(os.Stderr, "rollback mount succeeded\n")
		}
		return fmt.Errorf("failed to save mount record: %w", err)
	}
	fmt.Fprintf(os.Stdout, "saved mount state to %s\n", stateFilePath)

	return nil
}

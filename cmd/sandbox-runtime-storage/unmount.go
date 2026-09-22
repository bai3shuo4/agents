package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func newUnmountCmd() *cobra.Command {
	var opts struct {
		driver string
	}

	cmd := &cobra.Command{
		Use:   "unmount",
		Short: "Unmount all dynamic CSI volumes and clean up state",
		Long: `Read the persisted mount state and call NodeUnpublishVolume
for each registered mount. After successful unmount, the symlink
under /runtime-dynamic-volume is removed and the state file is
cleared.

If additional arguments are provided after "--", they are treated as a
user command and executed directly after unmount completes. This follows
the kubectl exec convention and allows chaining custom cleanup logic.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := runUnmount(opts.driver); err != nil {
				return err
			}
			if len(args) > 0 {
				c := exec.Command(args[0], args[1:]...)
				c.Stdout = os.Stdout
				c.Stderr = os.Stderr
				return c.Run()
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&opts.driver, "driver", "nfs.csi.k8s.io", "CSI driver name (e.g. nfs.csi.k8s.io)")
	return cmd
}

func runUnmount(driver string) error {
	// 1. Load state file
	state, err := loadState()
	if err != nil {
		return fmt.Errorf("failed to load state: %w", err)
	}

	if len(state.Mounts) == 0 {
		fmt.Println("no mount records found, nothing to unmount")
		return nil
	}

	// 2. Connect to CSI driver
	socketPath, err := resolveCSISocket(driver)
	if err != nil {
		return err
	}

	dialCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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

	nodeClient := csi.NewNodeClient(conn)

	// 3. Unmount each volume
	var successCount, failedCount int
	remainingMounts := make([]MountRecord, 0, len(state.Mounts))

	for _, record := range state.Mounts {
		fmt.Printf("unmounting volume %s (actual path: %s)...\n", record.VolumeId, record.ActualMountPath)

		req := &csi.NodeUnpublishVolumeRequest{
			VolumeId:   record.VolumeId,
			TargetPath: record.ActualMountPath,
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, err := nodeClient.NodeUnpublishVolume(ctx, req)
		cancel()

		if err != nil {
			fmt.Printf("  failed to unmount volume %s: %v\n", record.VolumeId, err)
			failedCount++
			remainingMounts = append(remainingMounts, record)
			continue
		}

		fmt.Printf("  unmounted volume %s successfully\n", record.VolumeId)
		successCount++

		// 4. Remove symlink under /runtime-dynamic-volume
		if record.SymlinkPath != "" {
			if rmErr := os.Remove(record.SymlinkPath); rmErr != nil && !os.IsNotExist(rmErr) {
				fmt.Printf("  warning: failed to remove symlink %s: %v\n", record.SymlinkPath, rmErr)
			} else {
				fmt.Printf("  removed symlink %s\n", record.SymlinkPath)
			}
		}

		// 5. Remove actual mount directory if empty to avoid accumulating orphaned directories.
		if record.ActualMountPath != "" {
			if rmErr := os.Remove(record.ActualMountPath); rmErr != nil {
				// The directory may not be empty if there are still files; this is expected and safe.
				if !os.IsNotExist(rmErr) {
					fmt.Printf("  warning: failed to remove actual mount directory %s: %v (directory may not be empty)\n", record.ActualMountPath, rmErr)
				}
			} else {
				fmt.Printf("  removed actual mount directory %s\n", record.ActualMountPath)
			}
		}
	}

	fmt.Printf("unmount summary: %d succeeded, %d failed, %d remaining\n",
		successCount, failedCount, len(remainingMounts))

	// 5. Update state file: keep only failed unmounts for retry
	if len(remainingMounts) == 0 {
		if err := clearState(); err != nil {
			fmt.Printf("warning: failed to clear state file: %v\n", err)
		} else {
			fmt.Println("state file cleared")
		}
	} else {
		state.Mounts = remainingMounts
		if err := saveState(state); err != nil {
			fmt.Printf("warning: failed to update state file: %v\n", err)
		}
	}

	return nil
}

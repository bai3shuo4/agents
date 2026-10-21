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
		driver      string
		inCSIDriver bool
		dryRun      bool
	}

	cmd := &cobra.Command{
		Use:   "unmount",
		Short: "Unmount all dynamic CSI volumes and clean up state",
		Long: `Read the persisted mount state and call NodeUnpublishVolume
for each registered mount. After successful unmount, the symlink
under /runtime-dynamic-volume is removed and the state file is
cleared.

By default --dry-run is enabled, meaning no actual unmount or cleanup
operations are performed. Use --dry-run=false to execute for real.

When running inside the CSI driver sidecar (--in-csi-driver), the
main container has already exited so no process is holding the mount.
In this mode symlinks are skipped (they live in the main container's
overlay filesystem which is invisible to the sidecar) and the CSI
socket is resolved at the driver-internal path (/csi/csi.sock).

If additional arguments are provided after "--", they are treated as a
user command and executed directly after unmount completes. This follows
the kubectl exec convention and allows chaining custom cleanup logic.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := runUnmount(opts.driver, opts.inCSIDriver, opts.dryRun); err != nil {
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
	cmd.Flags().BoolVar(&opts.inCSIDriver, "in-csi-driver", false, "Run inside the CSI driver sidecar (skips symlink cleanup, uses internal CSI socket)")
	cmd.Flags().BoolVar(&opts.dryRun, "dry-run", true, "Print operations without executing them. Set to false to perform actual unmount.")
	return cmd
}

func runUnmount(driver string, inCSIDriver bool, dryRun bool) error {
	// 1. Load state file
	state, err := loadState()
	if err != nil {
		return fmt.Errorf("failed to load state: %w", err)
	}

	if len(state.Mounts) == 0 {
		fmt.Println("no mount records found, nothing to unmount")
		return nil
	}

	if dryRun {
		fmt.Println("[DRY-RUN] No changes will be made. Use --dry-run=false to execute.")
	}

	// Print summary of planned operations
	fmt.Printf("found %d mount record(s) to process:\n", len(state.Mounts))
	for i, record := range state.Mounts {
		fmt.Printf("  %d. VolumeId=%s TargetPath=%s\n", i+1, record.VolumeId, record.ActualMountPath)
	}

	// 2. Resolve CSI socket (only connect in non-dry-run mode)
	var socketPath string
	if inCSIDriver {
		fmt.Println("running in CSI driver sidecar mode")
		socketPath, err = resolveCSISocketInDriver()
	} else {
		socketPath, err = resolveCSISocket(driver)
	}
	if err != nil {
		return err
	}
	fmt.Printf("CSI socket: %s\n", socketPath)

	var nodeClient csi.NodeClient
	if !dryRun {
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

		nodeClient = csi.NewNodeClient(conn)
	}

	// 3. Unmount each volume
	var successCount, failedCount int
	remainingMounts := make([]MountRecord, 0, len(state.Mounts))

	for _, record := range state.Mounts {
		fmt.Printf("unmounting volume %s (actual path: %s)...\n", record.VolumeId, record.ActualMountPath)

		req := &csi.NodeUnpublishVolumeRequest{
			VolumeId:   record.VolumeId,
			TargetPath: record.ActualMountPath,
		}

		if dryRun {
			fmt.Printf("  [DRY-RUN] would call NodeUnpublishVolume(VolumeId=%s, TargetPath=%s)\n",
				req.VolumeId, req.TargetPath)
		} else {
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
		}
		successCount++

		// 4. Remove symlink under /runtime-dynamic-volume (skipped in sidecar mode)
		if !inCSIDriver && record.SymlinkPath != "" {
			if dryRun {
				fmt.Printf("  [DRY-RUN] would remove symlink %s\n", record.SymlinkPath)
			} else {
				if rmErr := os.Remove(record.SymlinkPath); rmErr != nil && !os.IsNotExist(rmErr) {
					fmt.Printf("  warning: failed to remove symlink %s: %v\n", record.SymlinkPath, rmErr)
				} else {
					fmt.Printf("  removed symlink %s\n", record.SymlinkPath)
				}
			}
			} else if inCSIDriver && record.SymlinkPath != "" {
			fmt.Printf("  skipped symlink cleanup (sidecar mode): %s\n", record.SymlinkPath)
			}

			// 5. Remove actual mount directory if empty to avoid accumulating orphaned directories.
			if record.ActualMountPath != "" {
				if dryRun {
					fmt.Printf("  [DRY-RUN] would remove actual mount directory %s\n", record.ActualMountPath)
				} else {
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

			// 6. Remove per-mount state file on successful unmount.
			if !dryRun {
				statePath := recordStatePath(record)
				if rmErr := os.Remove(statePath); rmErr != nil && !os.IsNotExist(rmErr) {
					fmt.Printf("  warning: failed to remove state file %s: %v\n", statePath, rmErr)
				}
			}
			}

			fmt.Printf("unmount summary: %d succeeded, %d failed, %d remaining\n",
			successCount, failedCount, len(remainingMounts))

			// 7. Clean up state directory when all unmounts succeeded.
			if dryRun {
			fmt.Println("[DRY-RUN] state files unchanged")
			} else if len(remainingMounts) == 0 {
			if err := clearState(); err != nil {
				fmt.Printf("warning: failed to clear state directory: %v\n", err)
			} else {
				fmt.Println("state directory cleared")
			}
			} else {
			// State files for successful unmounts were already removed in the loop.
			// State files for failed unmounts remain intact for retry.
			fmt.Printf("kept %d state file(s) for failed unmounts\n", len(remainingMounts))
			}

	return nil
}

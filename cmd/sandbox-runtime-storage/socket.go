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
	"fmt"
	"os"
	"path/filepath"
)

const (
	// csiSocketBaseDir is where CSI sidecar init containers bind-mount their sockets.
	// Matches the VolumeMount MountPath injected by setCSIMountContainer:
	//   /var/run/csi/sockets/<driverName>
	csiSocketBaseDir = "/var/run/csi/sockets"
	csiSocketName    = "csi.sock"

	// csiDriverInternalSocket is the standard unix socket path inside a CSI driver container.
	// When running inside the CSI driver sidecar (via --in-csi-driver), the gRPC listener
	// is typically bound at /csi/csi.sock (e.g., nfs-subdir-external-provisioner).
	csiDriverInternalSocket = "/csi/csi.sock"
)

// resolveCSISocket returns the unix socket path for the given CSI driver.
// The CSI driver sidecar (injected as an init container) is expected to bind
// its gRPC listener at /var/run/csi/sockets/<driverName>/csi.sock.
func resolveCSISocket(driverName string) (string, error) {
	socketPath := filepath.Join(csiSocketBaseDir, driverName, csiSocketName)
	if _, err := os.Stat(socketPath); err != nil {
		return "", fmt.Errorf("csi socket not found at %s: %w", socketPath, err)
	}
	return socketPath, nil
}

// resolveCSISocketInDriver returns the unix socket path when running inside
// the CSI driver sidecar container itself. In this mode the socket is at a
// fixed well-known path (/csi/csi.sock) because the sidecar and the driver
// share the same network/pid namespace or volume mounts.
func resolveCSISocketInDriver() (string, error) {
	if _, err := os.Stat(csiDriverInternalSocket); err != nil {
		return "", fmt.Errorf("csi socket not found at %s: %w", csiDriverInternalSocket, err)
	}
	return csiDriverInternalSocket, nil
}

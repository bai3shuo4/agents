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

	"github.com/spf13/cobra"
)

func main() {
	// Attempt to rebuild mount state from existing symlinks before serving commands.
	// This compensates for cases where the state file was lost but mounts are still active.
	if err := rebuildStateFromSymlinks(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to rebuild state from symlinks: %v\n", err)
	}

	root := &cobra.Command{
		Use:          "sandbox-runtime-storage",
		Short:        "CSI volume mount helper for sandbox pods",
		SilenceUsage: true,
	}
	root.AddCommand(newMountCmd())
	root.AddCommand(newUnmountCmd())
	if err := root.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

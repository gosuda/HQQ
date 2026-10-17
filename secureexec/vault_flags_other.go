//go:build unix && !darwin && !freebsd

package secureexec

import "os"

// setVaultFlags is a no-op: this platform has no immutable file flag that is
// usable here. The vault relies on its 0700 directory and 0500 file.
func setVaultFlags(*os.File) {}

// clearVaultFlags is a no-op on this platform.
func clearVaultFlags(string) {}

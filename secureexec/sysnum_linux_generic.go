//go:build linux && (arm64 || riscv64 || loong64)

package secureexec

// memfdArchSupported reports whether sysMemfdCreate is known for this GOARCH.
const memfdArchSupported = true

// sysMemfdCreate is the memfd_create(2) number from the asm-generic table
// (linux/arm64, linux/riscv64, linux/loong64: 279).
const sysMemfdCreate = 279

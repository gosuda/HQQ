//go:build linux && !(386 || amd64 || arm || arm64 || loong64 || mips || mipsle || mips64 || mips64le || ppc64 || ppc64le || riscv64 || s390x)

package secureexec

// memfdArchSupported is false on GOARCH values whose memfd_create number is not
// listed here; Verify then uses the vault.
const memfdArchSupported = false

// sysMemfdCreate is unused when memfdArchSupported is false.
const sysMemfdCreate = 0

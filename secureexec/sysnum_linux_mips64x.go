//go:build linux && (mips64 || mips64le)

package secureexec

// memfdArchSupported reports whether sysMemfdCreate is known for this GOARCH.
const memfdArchSupported = true

// sysMemfdCreate is the memfd_create(2) number (linux/mips64, linux/mips64le n64 ABI: 5314).
const sysMemfdCreate = 5314

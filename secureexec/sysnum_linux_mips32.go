//go:build linux && (mips || mipsle)

package secureexec

// memfdArchSupported reports whether sysMemfdCreate is known for this GOARCH.
const memfdArchSupported = true

// sysMemfdCreate is the memfd_create(2) number (linux/mips, linux/mipsle o32 ABI: 4354).
const sysMemfdCreate = 4354

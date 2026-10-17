//go:build linux && 386

package secureexec

// memfdArchSupported reports whether sysMemfdCreate is known for this GOARCH.
const memfdArchSupported = true

// sysMemfdCreate is the memfd_create(2) number (linux/386: 356).
const sysMemfdCreate = 356

//go:build linux && amd64

package secureexec

// memfdArchSupported reports whether sysMemfdCreate is known for this GOARCH.
const memfdArchSupported = true

// sysMemfdCreate is the memfd_create(2) number (linux/amd64: 319).
const sysMemfdCreate = 319

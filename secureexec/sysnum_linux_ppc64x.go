//go:build linux && (ppc64 || ppc64le)

package secureexec

// memfdArchSupported reports whether sysMemfdCreate is known for this GOARCH.
const memfdArchSupported = true

// sysMemfdCreate is the memfd_create(2) number (linux/ppc64, linux/ppc64le: 360).
const sysMemfdCreate = 360

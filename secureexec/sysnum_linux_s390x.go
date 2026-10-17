//go:build linux && s390x

package secureexec

// memfdArchSupported reports whether sysMemfdCreate is known for this GOARCH.
const memfdArchSupported = true

// sysMemfdCreate is the memfd_create(2) number (linux/s390x: 350).
const sysMemfdCreate = 350

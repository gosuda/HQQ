package launcher

import (
	"bytes"
	"crypto/ed25519"
	_ "embed"
	"fmt"
	"strings"

	"gosuda.org/hqq/secureexec/manifest"
)

// releasePublicKeyFile is the release public key, as hex text: the maintainer
// replaces the placeholder with the output of `hqq-manifest keygen` before the
// first signed release. Until then ReleasePublicKey fails.
//
//go:embed release_public_key.hex
var releasePublicKeyFile string

// releasePlaceholder is the prefix of the placeholder that ships in the
// repository.
const releasePlaceholder = "PLACEHOLDER"

// ReleasePublicKey returns the release public key embedded in this package.
// It fails with ErrNoReleaseKey while the placeholder is still in place, and
// for any text that is not a 32-byte hex key; it never returns a zero key.
// Start uses it when ManifestSource.PublicKey is nil.
func ReleasePublicKey() (ed25519.PublicKey, error) {
	text := strings.TrimSpace(releasePublicKeyFile)
	if strings.HasPrefix(text, releasePlaceholder) {
		return nil, fmt.Errorf("%w: sqlite/launcher/release_public_key.hex is a placeholder; "+
			"run hqq-manifest keygen and commit the public key before the first signed release",
			ErrNoReleaseKey)
	}
	pub, err := manifest.ParsePublicKey([]byte(text))
	if err != nil {
		return nil, fmt.Errorf("%w: release_public_key.hex: %w", ErrNoReleaseKey, err)
	}
	if bytes.Equal(pub, make([]byte, ed25519.PublicKeySize)) {
		return nil, fmt.Errorf("%w: release_public_key.hex holds an all-zero key", ErrNoReleaseKey)
	}
	return pub, nil
}

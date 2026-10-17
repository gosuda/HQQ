//go:build unix && !linux

package secureexec

// protectMemfd is not available off Linux; (nil, nil) selects the vault.
func protectMemfd(string) (*protected, error) {
	return nil, nil
}

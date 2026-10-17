package wire

// ValidPragmaName reports whether name is an allowed PRAGMA name in Open:
// [a-z_][a-z0-9_]{0,63}. Only this alphabet is ever spliced into SQL.
func ValidPragmaName(name string) bool {
	if len(name) == 0 || len(name) > 64 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c == '_':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// ValidPragmaValue reports whether value is an allowed PRAGMA value in Open:
// up to 255 bytes of [A-Za-z0-9_.+-]. The empty value is allowed.
func ValidPragmaValue(value string) bool {
	if len(value) > 255 {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_', c == '.', c == '+', c == '-':
		default:
			return false
		}
	}
	return true
}

// ValidOpenFlags reports whether flags is a valid Open flag set: exactly one of
// OpenReadOnly and OpenReadWrite, optionally OpenCreate, and no other bits.
func ValidOpenFlags(flags uint32) bool {
	const known = OpenReadOnly | OpenReadWrite | OpenCreate
	if flags&^known != 0 {
		return false
	}
	access := flags & (OpenReadOnly | OpenReadWrite)
	return access == OpenReadOnly || access == OpenReadWrite
}

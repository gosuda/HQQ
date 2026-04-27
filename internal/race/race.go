//go:build !race

package race

func Enable() {}

func Disable() {}

func Do(fn func()) {
	fn()
}

func Copy(dst, src []byte) int {
	return copy(dst, src)
}

func Zero(b []byte) {
	clear(b)
}

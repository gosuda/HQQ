package gen

// File is one generated output file. Name is a base name without a directory.
type File struct {
	Name string
	Data []byte
}

// GoOptions configure the Go client emitter.
type GoOptions struct {
	// Package is the package clause of the generated file. Empty selects the
	// service name.
	Package string
	// Strip is removed from the start of every function name before it is
	// converted to a Go name.
	Strip string
	// Source is the IDL file name as the user gave it. It appears in the
	// header comment of the generated file.
	Source string
}

// COptions configure the C server emitter.
type COptions struct {
	// Source is the IDL file name as the user gave it. It appears in the
	// header comment of the generated files.
	Source string
}

package idl_test

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gosuda.org/hqq/bindgen/idl"
)

// FuzzParse is the target of 10.2: no panic, termination, every diagnostic carries a rule id and a position,
// and an accepted input yields a canonical form and a model that are stable between two runs.
func FuzzParse(f *testing.F) {
	for _, name := range []string{"microlib/microlib.idl", "microlib/microlib_v2.idl", "microlib/microbench.idl", "sqlite_lowlevel.idl"} {
		data, err := os.ReadFile(filepath.Join("..", "testdata", filepath.FromSlash(name)))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data, "ml_")
	}
	f.Add([]byte(everything), "thing_")
	f.Add([]byte(typesIDL), "")
	for _, group := range allNegativeCases() {
		for _, c := range group {
			src := c.src
			if !c.raw {
				src = hdr + src
			}
			src = strings.Replace(src, marker, "", 1)
			if len(src) > 4096 {
				continue
			}
			f.Add([]byte(src), c.opts.GoStrip)
		}
	}

	f.Fuzz(func(t *testing.T, src []byte, strip string) {
		opts := idl.Options{GoStrip: strip}
		spec, err := idl.Parse("fuzz.idl", src, opts)
		spec2, err2 := idl.Parse("fuzz.idl", src, opts)
		if (err == nil) != (err2 == nil) {
			t.Fatalf("two parses disagree on acceptance: %v / %v", err, err2)
		}
		if err != nil {
			ds, ok := err.(idl.Diagnostics)
			if !ok || len(ds) == 0 {
				t.Fatalf("error %T %v is not non-empty Diagnostics", err, err)
			}
			if err.Error() != err2.Error() {
				t.Fatalf("two parses give different diagnostics:\n%v\n%v", err, err2)
			}
			for _, d := range ds {
				if d.Rule == "" || d.Line < 1 || d.Col < 1 || d.Msg == "" || d.File != "fuzz.idl" {
					t.Fatalf("incomplete diagnostic %+v", d)
				}
			}
			if spec != nil {
				t.Fatal("a Spec came with an error")
			}
			return
		}
		c1, c2 := spec.Canonical(), spec2.Canonical()
		if string(c1) != string(c2) || string(spec.Canonical()) != string(c1) {
			t.Fatal("Canonical is not stable")
		}
		if spec.Digest() != sha256.Sum256(c1) {
			t.Fatal("Digest is not the hash of Canonical")
		}
		if len(c1) == 0 || c1[len(c1)-1] != '\n' || !strings.HasPrefix(string(c1), "HQQ-IDL 1\nservice ") {
			t.Fatalf("malformed canonical text %q", c1)
		}
		for i, fn := range spec.Wire {
			if fn.Opcode != i+1 || fn.Plan == nil {
				t.Fatalf("wire function %s: opcode %d, plan %v", fn.Name, fn.Opcode, fn.Plan)
			}
			bound, err := fn.Plan.StaticBound(fn)
			if err != nil || bound > idl.MaxStatic {
				t.Fatalf("%s: static bound %d, %v", fn.Name, bound, err)
			}
			for _, l := range []idl.Layout{fn.Plan.Req, fn.Plan.Resp} {
				prev := 0
				for _, fl := range l.Fixed {
					if fl.Off < prev || fl.Off+1 > l.FixedSize+1 {
						t.Fatalf("%s: field %s at %d, previous end %d, size %d", fn.Name, fl.Name, fl.Off, prev, l.FixedSize)
					}
					size, align, fixed := idl.SizeOf(fl.Type)
					if !fixed || fl.Off%int(align) != 0 {
						t.Fatalf("%s: field %s misplaced", fn.Name, fl.Name)
					}
					prev = fl.Off + int(size)
				}
				if prev != l.FixedSize || l.FixedPad < l.FixedSize || l.FixedPad%8 != 0 || l.FixedPad-l.FixedSize >= 8 {
					t.Fatalf("%s: layout sizes %d %d %d", fn.Name, prev, l.FixedSize, l.FixedPad)
				}
			}
		}
		for _, s := range spec.Structs {
			ty := &idl.Type{Kind: idl.KStruct, Name: s.Name, St: s}
			size, align, fixed := idl.SizeOf(ty)
			if fixed && (size <= 0 || size%align != 0 || size > idl.MaxStatic) {
				t.Fatalf("struct %s: size %d align %d", s.Name, size, align)
			}
			idl.MinSize(ty)
			idl.WorstCase(ty)
		}
	})
}

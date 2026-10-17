// Command benchtable turns the output of `go test -bench` into a table: one row per benchmark path
// without its last element, one column per last element, one cell per metric value. For the
// benchmarks of bindgen/bench that is a row per payload size or loop count and a column per
// implementation (cgo, hqq-sync, hqq-pipe-8, ...), so the crossover of BINDGEN.md 10.5 reads off one
// line.
//
//	go test -bench Work -benchtime 2s -count 5 ./bindgen/bench | go run ./bindgen/bench/benchtable
//	go run ./bindgen/bench/benchtable -metric calls/s -match '^Xor/' bench.txt
//
// A benchmark that ran more than once (-count) shows the median of its values. The GOMAXPROCS suffix
// that the testing package appends to the last element ("-8") is removed when every line carries
// the same one. Lines that are not benchmark results are ignored; a result without the metric is
// shown as "-". The output of benchstat is the tool for statistics across runs; this one is for the
// layout.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "benchtable:", err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("benchtable", flag.ContinueOnError)
	fs.SetOutput(stderr)
	metric := fs.String("metric", "ns/op", "the `unit` to show: ns/op, calls/s, MB/s, B/op, allocs/op or any other reported unit")
	match := fs.String("match", "", "show only benchmarks whose name (without the Benchmark prefix) matches this `regexp`")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var re *regexp.Regexp
	if *match != "" {
		var err error
		if re, err = regexp.Compile(*match); err != nil {
			return fmt.Errorf("-match: %w", err)
		}
	}
	var results []result
	if fs.NArg() == 0 {
		rs, err := parse(stdin)
		if err != nil {
			return err
		}
		results = rs
	}
	for _, path := range fs.Args() {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		rs, err := parse(f)
		err = errors.Join(err, f.Close())
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		results = append(results, rs...)
	}
	results = stripProcs(results)
	if re != nil {
		results = slices.DeleteFunc(results, func(r result) bool { return !re.MatchString(r.name) })
	}
	if len(results) == 0 {
		return errors.New("no benchmark results in the input")
	}
	return render(stdout, results, *metric)
}

// result is one benchmark line: its name without the Benchmark prefix and its metrics by unit.
type result struct {
	name    string
	metrics map[string]float64
}

var procsSuffix = regexp.MustCompile(`-\d+$`)

// parse reads the benchmark lines of r (the format of testing: name, iteration count, then value and
// unit pairs).
func parse(r io.Reader) ([]result, error) {
	var out []result
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 4 || !strings.HasPrefix(f[0], "Benchmark") || (len(f)-2)%2 != 0 {
			continue
		}
		if _, err := strconv.ParseInt(f[1], 10, 64); err != nil {
			continue
		}
		res := result{name: strings.TrimPrefix(f[0], "Benchmark"), metrics: map[string]float64{}}
		ok := true
		for i := 2; i < len(f); i += 2 {
			v, err := strconv.ParseFloat(f[i], 64)
			if err != nil {
				ok = false
				break
			}
			res.metrics[f[i+1]] = v
		}
		if ok {
			out = append(out, res)
		}
	}
	return out, sc.Err()
}

// stripProcs removes the GOMAXPROCS suffix when all results share it.
func stripProcs(rs []result) []result {
	if len(rs) == 0 {
		return rs
	}
	suffix := procsSuffix.FindString(rs[0].name)
	if suffix == "" {
		return rs
	}
	for _, r := range rs {
		if !strings.HasSuffix(r.name, suffix) {
			return rs
		}
	}
	for i := range rs {
		rs[i].name = strings.TrimSuffix(rs[i].name, suffix)
	}
	return rs
}

// splitLast splits a benchmark name at its last "/": the row and the column.
func splitLast(name string) (row, col string) {
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		return name[:i], name[i+1:]
	}
	return name, "-"
}

// render prints the table for metric: rows and columns in order of first appearance.
func render(w io.Writer, rs []result, metric string) error {
	var rows, cols []string
	cells := map[[2]string][]float64{}
	for _, r := range rs {
		row, col := splitLast(r.name)
		if !slices.Contains(rows, row) {
			rows = append(rows, row)
		}
		if !slices.Contains(cols, col) {
			cols = append(cols, col)
		}
		if v, ok := r.metrics[metric]; ok {
			k := [2]string{row, col}
			cells[k] = append(cells[k], v)
		}
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintf(tw, "%s\t", metric)
	for _, c := range cols {
		fmt.Fprintf(tw, "%s\t", c)
	}
	fmt.Fprintln(tw)
	for _, row := range rows {
		fmt.Fprintf(tw, "%s\t", row)
		for _, c := range cols {
			if vs := cells[[2]string{row, c}]; len(vs) > 0 {
				fmt.Fprintf(tw, "%s\t", format(median(vs)))
			} else {
				fmt.Fprint(tw, "-\t")
			}
		}
		fmt.Fprintln(tw)
	}
	return tw.Flush()
}

func median(vs []float64) float64 {
	s := slices.Clone(vs)
	slices.Sort(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// format prints v with three significant digits, as an integer when it is large.
func format(v float64) string {
	switch {
	case v >= 100:
		return strconv.FormatFloat(v, 'f', 0, 64)
	case v >= 10:
		return strconv.FormatFloat(v, 'f', 1, 64)
	default:
		return strconv.FormatFloat(v, 'f', 2, 64)
	}
}

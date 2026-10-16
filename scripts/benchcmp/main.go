// Command benchcmp compares two outputs of "go test -bench" (doc/benchmarks.md):
// for every benchmark and metric it takes the median of the runs (-count),
// prints old, new and the change, and exits with status 1 if a metric got
// worse by more than the threshold (default 10 %). Lower is better for
// ns/op, B/op and allocs/op; higher is better for MB/s and */s metrics.
//
// Usage: go run ./scripts/benchcmp [-threshold 10] old.txt new.txt
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

func main() {
	threshold := flag.Float64("threshold", 10, "regression threshold in percent")
	flag.Parse()
	if flag.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "usage: benchcmp [-threshold 10] old.txt new.txt")
		os.Exit(2)
	}
	old, err := parse(flag.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	cur, err := parse(flag.Arg(1))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	var keys []string
	for k := range cur {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	regressed := false
	fmt.Printf("%-40s %14s %14s %8s\n", "benchmark metric", "old", "new", "change")
	for _, k := range keys {
		o, ok := old[k]
		if !ok {
			fmt.Printf("%-40s %14s %14.4g %8s\n", k, "-", median(cur[k]), "new")
			continue
		}
		mo, mn := median(o), median(cur[k])
		change := (mn - mo) / mo * 100
		worse := change
		if higherIsBetter(k) {
			worse = -change
		}
		mark := ""
		if worse > *threshold {
			mark = "  REGRESSION"
			regressed = true
		}
		fmt.Printf("%-40s %14.4g %14.4g %+7.1f%%%s\n", k, mo, mn, change, mark)
	}
	if regressed {
		fmt.Printf("regressions above %.0f%%\n", *threshold)
		os.Exit(1)
	}
}

var line = regexp.MustCompile(`^(Benchmark\S+?)(?:-\d+)?\s+\d+\s+(.*)$`)

// parse reads benchmark results: "<name> <metric unit>" → values.
func parse(path string) (map[string][]float64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string][]float64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		m := line.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		fields := strings.Fields(m[2])
		for i := 0; i+1 < len(fields); i += 2 {
			v, err := strconv.ParseFloat(fields[i], 64)
			if err != nil {
				continue
			}
			key := m[1] + " " + fields[i+1]
			out[key] = append(out[key], v)
		}
	}
	return out, sc.Err()
}

func median(v []float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

func higherIsBetter(key string) bool {
	unit := key[strings.LastIndexByte(key, ' ')+1:]
	return unit == "MB/s" || strings.HasSuffix(unit, "/s")
}

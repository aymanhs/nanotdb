package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aymanhs/nanotdb/internal/engine"
)

type runStats struct {
	run          int
	database     string
	points       int
	importBytes  int
	totalDat     int64
	ratio        float64
	bytesPerPt   float64
	dailyFiles   int
	importPath   string
	exportPath   string
	dbDir        string
	daySizeStats []daySize
}

func main() {
	root := flag.String("root", filepath.Join("devdata"), "Root data directory")
	importPath := flag.String("import", filepath.Join("devdata", "import-large.lp"), "Input LP file path")
	exportPath := flag.String("export", filepath.Join("devdata", "export.lp"), "Output LP file path")
	database := flag.String("db", "prod", "Database to export and compare")
	walSize := flag.Int64("wal-max", 1024*1024, "WAL max segment size")

	generate := flag.Bool("generate", false, "Generate import LP inside this tester")
	runs := flag.Int("runs", 1, "Number of repeated runs")
	dbCount := flag.Int("db-count", 2, "Databases in generated import")
	metrics := flag.Int("metrics", 10, "Metrics per database in generated import")
	pointsPerMetric := flag.Int("points-per-metric", 50000, "Points per metric in generated import")
	intervalSec := flag.Float64("interval-sec", 10.0, "Base interval seconds for generated import")
	jitterSec := flag.Float64("jitter-sec", 1.5, "Uniform +/- jitter seconds for generated import")
	startTSNs := flag.Int64("start-ts-ns", 1_700_000_000_000_000_000, "Start timestamp (unix nanos) for generated import")
	seedBase := flag.Int64("seed", 42, "Base RNG seed (seed+runIndex per run)")
	floatRatio := flag.Float64("float-ratio", 0.0, "Fraction of metrics emitted as float in generated import [0,1]")
	dbPrefix := flag.String("db-prefix", "prod", "DB prefix for generated import: prod, prod_1, ...")

	flag.Parse()

	if *runs < 1 {
		fail(fmt.Errorf("-runs must be >= 1"))
	}
	if *floatRatio < 0 || *floatRatio > 1 {
		fail(fmt.Errorf("-float-ratio must be in [0,1]"))
	}

	all := make([]runStats, 0, *runs)
	for i := 1; i <= *runs; i++ {
		stats, err := runOnce(
			i,
			*root,
			*importPath,
			*exportPath,
			*database,
			*walSize,
			*generate,
			*dbCount,
			*metrics,
			*pointsPerMetric,
			*intervalSec,
			*jitterSec,
			*startTSNs,
			*seedBase+int64(i-1),
			*floatRatio,
			*dbPrefix,
		)
		if err != nil {
			fail(err)
		}
		all = append(all, stats)
		printStats(stats)
	}

	if len(all) > 1 {
		printSummary(all)
	}
}

func runOnce(
	run int,
	rootPath, importPathBase, exportPathBase, database string,
	walSize int64,
	generate bool,
	dbCount, metrics, pointsPerMetric int,
	intervalSec, jitterSec float64,
	startTSNs, seed int64,
	floatRatio float64,
	dbPrefix string,
) (runStats, error) {
	importPath := withRunSuffix(importPathBase, run)
	exportPath := withRunSuffix(exportPathBase, run)

	if err := os.MkdirAll(rootPath, 0755); err != nil {
		return runStats{}, err
	}

	if generate {
		if err := generateImportLP(importPath, dbCount, metrics, pointsPerMetric, intervalSec, jitterSec, startTSNs, seed, floatRatio, dbPrefix); err != nil {
			return runStats{}, err
		}
	}
	if _, err := os.Stat(importPath); err != nil {
		return runStats{}, fmt.Errorf("import file not found: %s", importPath)
	}

	importRaw, err := os.ReadFile(importPath)
	if err != nil {
		return runStats{}, err
	}

	for _, db := range databasesFromImport(string(importRaw)) {
		_ = os.RemoveAll(filepath.Join(rootPath, db))
	}
	dbDir := filepath.Join(rootPath, database)
	_ = os.Remove(exportPath)

	e, err := engine.OpenEngine(rootPath, walSize)
	if err != nil {
		return runStats{}, err
	}
	if err := e.ImportFile(importPath); err != nil {
		_ = e.Close()
		return runStats{}, err
	}
	if err := e.Close(); err != nil {
		return runStats{}, err
	}

	e2, err := engine.OpenEngine(rootPath, walSize)
	if err != nil {
		return runStats{}, err
	}
	defer e2.Close()

	if err := e2.ExportFile(database, exportPath); err != nil {
		return runStats{}, err
	}

	wantDB := filterImportForDB(string(importRaw), database)
	got, err := os.ReadFile(exportPath)
	if err != nil {
		return runStats{}, err
	}
	if !bytes.Equal([]byte(wantDB), got) {
		return runStats{}, fmt.Errorf("run %d mismatch: import(db=%s)=%d bytes export=%d bytes", run, database, len(wantDB), len(got))
	}

	points := countNonEmptyLines(wantDB)
	importBytes := len(wantDB)
	totalDat, daySizes, err := datFileStats(dbDir)
	if err != nil {
		return runStats{}, err
	}
	ratio := 0.0
	if importBytes > 0 {
		ratio = float64(totalDat) / float64(importBytes)
	}
	bytesPerPt := 0.0
	if points > 0 {
		bytesPerPt = float64(totalDat) / float64(points)
	}

	return runStats{
		run:          run,
		database:     database,
		points:       points,
		importBytes:  importBytes,
		totalDat:     totalDat,
		ratio:        ratio,
		bytesPerPt:   bytesPerPt,
		dailyFiles:   len(daySizes),
		importPath:   importPath,
		exportPath:   exportPath,
		dbDir:        dbDir,
		daySizeStats: daySizes,
	}, nil
}

func printStats(s runStats) {
	fmt.Printf("OK run %d: import/export round-trip is identical\n", s.run)
	fmt.Printf("import: %s\n", s.importPath)
	fmt.Printf("export: %s\n", s.exportPath)
	fmt.Printf("db dir: %s\n", s.dbDir)
	fmt.Println("--- compression stats ---")
	fmt.Printf("db: %s\n", s.database)
	fmt.Printf("points: %d\n", s.points)
	fmt.Printf("import payload bytes: %d\n", s.importBytes)
	fmt.Printf("total .dat bytes: %d\n", s.totalDat)
	fmt.Printf("dat/import ratio: %.4f\n", s.ratio)
	fmt.Printf("bytes per point on disk: %.4f\n", s.bytesPerPt)
	fmt.Printf("daily files: %d\n", s.dailyFiles)
	for _, ds := range s.daySizeStats {
		fmt.Printf("  %s: %d bytes\n", ds.name, ds.size)
	}
}

func printSummary(all []runStats) {
	minRatio, maxRatio, sumRatio := all[0].ratio, all[0].ratio, 0.0
	minBpp, maxBpp, sumBpp := all[0].bytesPerPt, all[0].bytesPerPt, 0.0
	for _, s := range all {
		sumRatio += s.ratio
		sumBpp += s.bytesPerPt
		if s.ratio < minRatio {
			minRatio = s.ratio
		}
		if s.ratio > maxRatio {
			maxRatio = s.ratio
		}
		if s.bytesPerPt < minBpp {
			minBpp = s.bytesPerPt
		}
		if s.bytesPerPt > maxBpp {
			maxBpp = s.bytesPerPt
		}
	}
	avgRatio := sumRatio / float64(len(all))
	avgBpp := sumBpp / float64(len(all))

	fmt.Println("=== summary ===")
	fmt.Printf("runs: %d\n", len(all))
	fmt.Printf("ratio min/avg/max: %.4f / %.4f / %.4f\n", minRatio, avgRatio, maxRatio)
	fmt.Printf("bytes/pt min/avg/max: %.4f / %.4f / %.4f\n", minBpp, avgBpp, maxBpp)
}

func withRunSuffix(path string, run int) string {
	if run == 1 {
		return path
	}
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	return fmt.Sprintf("%s.run%d%s", base, run, ext)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "ERROR:", err)
	os.Exit(1)
}

func filterImportForDB(body, db string) string {
	prefix := db + "/"
	lines := strings.Split(body, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, prefix) {
			kept = append(kept, line)
		}
	}
	if len(kept) == 0 {
		return ""
	}
	return strings.Join(kept, "\n") + "\n"
}

func databasesFromImport(body string) []string {
	set := make(map[string]struct{})
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		firstSpace := strings.IndexByte(line, ' ')
		if firstSpace <= 0 {
			continue
		}
		target := line[:firstSpace]
		slash := strings.IndexByte(target, '/')
		if slash <= 0 {
			continue
		}
		set[target[:slash]] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for db := range set {
		out = append(out, db)
	}
	return out
}

func countNonEmptyLines(body string) int {
	n := 0
	for _, line := range strings.Split(body, "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

type daySize struct {
	name string
	size int64
}

func datFileStats(dbDir string) (int64, []daySize, error) {
	entries, err := os.ReadDir(dbDir)
	if err != nil {
		return 0, nil, err
	}
	total := int64(0)
	list := make([]daySize, 0)
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		name := ent.Name()
		if !strings.HasPrefix(name, "data-") || !strings.HasSuffix(name, ".dat") {
			continue
		}
		info, err := ent.Info()
		if err != nil {
			return 0, nil, err
		}
		sz := info.Size()
		total += sz
		list = append(list, daySize{name: name, size: sz})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].name < list[j].name })
	return total, list, nil
}

func generateImportLP(
	outPath string,
	dbCount int,
	metrics int,
	pointsPerMetric int,
	intervalSec float64,
	jitterSec float64,
	startTSNs int64,
	seed int64,
	floatRatio float64,
	dbPrefix string,
) error {
	if dbCount < 1 || metrics < 1 || pointsPerMetric < 1 {
		return fmt.Errorf("invalid generation params")
	}
	if intervalSec <= 0 {
		return fmt.Errorf("interval-sec must be > 0")
	}
	if floatRatio < 0 || floatRatio > 1 {
		return fmt.Errorf("float-ratio must be in [0,1]")
	}

	if err := os.MkdirAll(filepath.Dir(outPath), 0755); err != nil {
		return err
	}
	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriterSize(f, 1<<20)
	defer w.Flush()

	rng := rand.New(rand.NewSource(seed))
	intervalNS := int64(intervalSec * 1e9)
	jitterNS := int64(jitterSec * 1e9)

	dbNames := make([]string, 0, dbCount)
	for i := 0; i < dbCount; i++ {
		if i == 0 {
			dbNames = append(dbNames, dbPrefix)
		} else {
			dbNames = append(dbNames, fmt.Sprintf("%s_%d", dbPrefix, i))
		}
	}

	metricNames := make([]string, metrics)
	for i := 0; i < metrics; i++ {
		metricNames[i] = fmt.Sprintf("sensors.room%d.metric%02d", i%5+1, i+1)
	}

	for dbIdx, db := range dbNames {
		ts := startTSNs + int64(dbIdx)*int64(5e8)
		baselines := make([]float64, metrics)
		for i := 0; i < metrics; i++ {
			baselines[i] = 2000.0 + float64(i)*13.0 + (rng.Float64()*10 - 5)
		}

		for step := 0; step < pointsPerMetric; step++ {
			jit := rng.Int63n(2*jitterNS+1) - jitterNS
			delta := intervalNS + jit
			if delta < 1 {
				delta = 1
			}
			ts += delta

			for mIdx, metric := range metricNames {
				drift := 0.003 * float64(step)
				wave := 7.5 * math.Sin(float64(step)/60.0+float64(mIdx)*0.3)
				noise := rng.Float64()*2.4 - 1.2
				value := baselines[mIdx] + drift + wave + noise

				floatCut := int(floatRatio * float64(metrics))
				if mIdx < floatCut {
					if _, err := fmt.Fprintf(w, "%s/%s %.4f %d\n", db, metric, value, ts); err != nil {
						return err
					}
				} else {
					iv := int(math.Round(value))
					if _, err := fmt.Fprintf(w, "%s/%s %d %d\n", db, metric, iv, ts); err != nil {
						return err
					}
				}
			}
		}
	}

	return nil
}

#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(cd -- "$script_dir/.." && pwd)

root=${1:-"$repo_root/test-data/metric-poc-big"}
db=${2:-"sensors"}

tmp_go=$(mktemp "$script_dir/metric_regen_tmp_XXXXXX.go")
cleanup() {
  rm -f "$tmp_go"
}
trap cleanup EXIT

cat >"$tmp_go" <<'EOF'
package main

import (
  "fmt"
  "os"
  "path/filepath"
  "sort"
  "strings"

  "github.com/aymanhs/nanotdb/internal/engine"
)

func main() {
  if len(os.Args) != 3 {
    fmt.Fprintf(os.Stderr, "usage: %s <root> <db>\n", os.Args[0])
    os.Exit(2)
  }
  root := os.Args[1]
  db := strings.Trim(strings.TrimSpace(os.Args[2]), "/")
  if db == "" {
    fmt.Fprintln(os.Stderr, "database cannot be empty")
    os.Exit(2)
  }

  dbDir := filepath.Join(root, db)
  dataFiles, err := filepath.Glob(filepath.Join(dbDir, "data-*.dat"))
  if err != nil {
    fmt.Fprintln(os.Stderr, err)
    os.Exit(1)
  }
  sort.Strings(dataFiles)
  if len(dataFiles) == 0 {
    fmt.Fprintf(os.Stderr, "no data files found under %s\n", dbDir)
    os.Exit(1)
  }

  eng, err := engine.OpenEngine(root, 1024*1024)
  if err != nil {
    fmt.Fprintln(os.Stderr, err)
    os.Exit(1)
  }
  defer eng.Close()

  for _, dataPath := range dataFiles {
    base := filepath.Base(dataPath)
    partition := strings.TrimSuffix(strings.TrimPrefix(base, "data-"), ".dat")
    metricPath := filepath.Join(dbDir, "metric-"+partition+".dat")
    if err := os.Remove(metricPath); err != nil && !os.IsNotExist(err) {
      fmt.Fprintf(os.Stderr, "remove %s: %v\n", metricPath, err)
      os.Exit(1)
    }

    builtPath, err := eng.BuildMetricFileV1(db, partition)
    if err != nil {
      fmt.Fprintf(os.Stderr, "build %s: %v\n", partition, err)
      os.Exit(1)
    }

    dataStat, err := os.Stat(dataPath)
    if err != nil {
      fmt.Fprintf(os.Stderr, "stat %s: %v\n", dataPath, err)
      os.Exit(1)
    }
    metricStat, err := os.Stat(builtPath)
    if err != nil {
      fmt.Fprintf(os.Stderr, "stat %s: %v\n", builtPath, err)
      os.Exit(1)
    }
    fmt.Printf("%s -> %s  data=%d metric=%d\n", base, filepath.Base(builtPath), dataStat.Size(), metricStat.Size())
  }
}
EOF

cd "$repo_root"
go run "$tmp_go" "$root" "$db"
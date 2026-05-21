package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunCSV2NLP_SortsRowsAndAppliesMetadata(t *testing.T) {
	root := t.TempDir()
	inDir := filepath.Join(root, "vm_export")
	if err := os.MkdirAll(inDir, 0755); err != nil {
		t.Fatalf("MkdirAll vm_export failed: %v", err)
	}

	cpuCSV := "\"metric\",sensor,__value__,__timestamp__\n" +
		",,89.25,4000\n" +
		",,96.5,1000\n"
	iowaitCSV := "\"metric\",sensor,__value__,__timestamp__\n" +
		",,0,2500\n" +
		",,0.20080321285124,2000\n"
	if err := os.WriteFile(filepath.Join(inDir, "cpu_usage_iowait.csv"), []byte(iowaitCSV), 0644); err != nil {
		t.Fatalf("WriteFile iowait csv failed: %v", err)
	}

	if err := os.WriteFile(filepath.Join(inDir, "cpu_usage_idle.csv"), []byte(cpuCSV), 0644); err != nil {
		t.Fatalf("WriteFile cpu csv failed: %v", err)
	}

	onewireCSV := "\"metric\",sensor,__value__,__timestamp__\n" +
		",out_dry,37.75,1500\n"
	if err := os.WriteFile(filepath.Join(inDir, "onewire_temperature.csv"), []byte(onewireCSV), 0644); err != nil {
		t.Fatalf("WriteFile onewire csv failed: %v", err)
	}

	skipCSV := "\"metric\",sensor,__value__,__timestamp__\n" +
		",,1,2000\n"
	if err := os.WriteFile(filepath.Join(inDir, "skip_me.csv"), []byte(skipCSV), 0644); err != nil {
		t.Fatalf("WriteFile skip csv failed: %v", err)
	}

	meta := `{
	  "metrics": {
	    "onewire_temperature": {"value_type": "int", "scale": 1000},
	    "skip_me": {"enabled": false}
	  }
	}`
	metaPath := filepath.Join(root, "metadata.json")
	if err := os.WriteFile(metaPath, []byte(meta), 0644); err != nil {
		t.Fatalf("WriteFile metadata failed: %v", err)
	}

	outPath := filepath.Join(root, "out.nlp")
	if err := runCSV2NLP([]string{"--in-dir", inDir, "--out", outPath, "--db", "vmprod", "--meta", metaPath}); err != nil {
		t.Fatalf("runCSV2NLP failed: %v", err)
	}

	raw, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("ReadFile out.nlp failed: %v", err)
	}
	got := strings.TrimSpace(string(raw))
	want := strings.Join([]string{
		"vmprod/cpu_usage_idle 96.5 1000000000",
		"vmprod/onewire_temperature.out_dry 37750i 1500000000",
		"vmprod/cpu_usage_iowait 0.20080321285124 2000000000",
		"vmprod/cpu_usage_iowait 0 2500000000",
		"vmprod/cpu_usage_idle 89.25 4000000000",
	}, "\n")
	if got != want {
		t.Fatalf("output mismatch:\nwant:\n%s\n\ngot:\n%s", want, got)
	}
}

func TestRunCSV2NLP_AppendSkipsExistingTail(t *testing.T) {
	root := t.TempDir()
	inDir := filepath.Join(root, "vm_export")
	if err := os.MkdirAll(inDir, 0755); err != nil {
		t.Fatalf("MkdirAll vm_export failed: %v", err)
	}

	cpuCSV := "\"metric\",sensor,__value__,__timestamp__\n" +
		",,1,1000\n" +
		",,2,2000\n" +
		",,3,3000\n"
	if err := os.WriteFile(filepath.Join(inDir, "cpu_usage_idle.csv"), []byte(cpuCSV), 0644); err != nil {
		t.Fatalf("WriteFile cpu csv failed: %v", err)
	}

	outPath := filepath.Join(root, "out.nlp")
	if err := os.WriteFile(outPath, []byte("vmprod/cpu_usage_idle 1i 1000000000\nvmprod/cpu_usage_idle 2i 2000000000\n"), 0644); err != nil {
		t.Fatalf("WriteFile out.nlp failed: %v", err)
	}

	if err := runCSV2NLP([]string{"--in-dir", inDir, "--out", outPath, "--db", "vmprod", "--append"}); err != nil {
		t.Fatalf("runCSV2NLP append failed: %v", err)
	}

	raw, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("ReadFile out.nlp failed: %v", err)
	}
	got := strings.TrimSpace(string(raw))
	want := strings.Join([]string{
		"vmprod/cpu_usage_idle 1i 1000000000",
		"vmprod/cpu_usage_idle 2i 2000000000",
		"vmprod/cpu_usage_idle 3i 3000000000",
	}, "\n")
	if got != want {
		t.Fatalf("append output mismatch:\nwant:\n%s\n\ngot:\n%s", want, got)
	}
}

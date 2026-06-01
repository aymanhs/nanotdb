package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteCatalog_PersistsAndClearsDirty verifies WriteCatalog() writes to the
// canonical file path established at LoadCatalog time and clears the dirty
// flag.
func TestWriteCatalog_PersistsAndClearsDirty(t *testing.T) {
	dir := t.TempDir()
	catalogPath := filepath.Join(dir, "catalog.json")
	c, err := LoadCatalog(catalogPath)
	if err != nil {
		t.Fatalf("LoadCatalog failed: %v", err)
	}
	defer c.Close()

	if err := c.EnsureMetricEntry("metric.a", 1, Float32Sample); err != nil {
		t.Fatalf("EnsureMetricEntry failed: %v", err)
	}
	if !c.IsDirty() {
		t.Fatal("expected catalog to be dirty after insert")
	}
	if err := c.WriteCatalog(); err != nil {
		t.Fatalf("WriteCatalog failed: %v", err)
	}
	if c.IsDirty() {
		t.Fatal("expected dirty flag cleared after WriteCatalog")
	}
	if _, err := os.Stat(catalogPath); err != nil {
		t.Fatalf("expected catalog file to exist: %v", err)
	}
}

// TestWriteCatalogTo_RefusesEmptyPath verifies WriteCatalogTo rejects an empty
// path rather than silently falling back to the canonical file (which would
// duplicate WriteCatalog's behaviour).
func TestWriteCatalogTo_RefusesEmptyPath(t *testing.T) {
	dir := t.TempDir()
	c, err := LoadCatalog(filepath.Join(dir, "catalog.json"))
	if err != nil {
		t.Fatalf("LoadCatalog failed: %v", err)
	}
	defer c.Close()

	if err := c.WriteCatalogTo(""); err == nil {
		t.Fatal("expected WriteCatalogTo(\"\") to fail")
	}
}

// TestWriteCatalogTo_RefusesCanonicalPath verifies WriteCatalogTo refuses to
// write to the canonical file path — that would bypass the fd refresh logic in
// WriteCatalog and leave the live engine reading from a stale inode.
func TestWriteCatalogTo_RefusesCanonicalPath(t *testing.T) {
	dir := t.TempDir()
	catalogPath := filepath.Join(dir, "catalog.json")
	c, err := LoadCatalog(catalogPath)
	if err != nil {
		t.Fatalf("LoadCatalog failed: %v", err)
	}
	defer c.Close()

	err = c.WriteCatalogTo(catalogPath)
	if err == nil {
		t.Fatal("expected WriteCatalogTo(canonical) to fail")
	}
	if !strings.Contains(err.Error(), "refusing to overwrite canonical") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestWriteCatalogTo_ExternalPath_DoesNotMutateCanonicalBinding is the core
// regression test for the bug: WriteCatalogTo to an external path must NOT
// rebind c.file. After the snapshot, WriteCatalog() must still write to the
// original canonical file.
func TestWriteCatalogTo_ExternalPath_DoesNotMutateCanonicalBinding(t *testing.T) {
	dir := t.TempDir()
	canonical := filepath.Join(dir, "catalog.json")
	external := filepath.Join(dir, "snapshot", "catalog.json")

	c, err := LoadCatalog(canonical)
	if err != nil {
		t.Fatalf("LoadCatalog failed: %v", err)
	}
	defer c.Close()

	if err := c.EnsureMetricEntry("metric.a", 1, Float32Sample); err != nil {
		t.Fatalf("EnsureMetricEntry failed: %v", err)
	}
	if err := c.WriteCatalogTo(external); err != nil {
		t.Fatalf("WriteCatalogTo failed: %v", err)
	}
	if _, err := os.Stat(external); err != nil {
		t.Fatalf("expected external snapshot to exist: %v", err)
	}

	// Snapshot must NOT clear the dirty flag — WriteCatalog() against the
	// canonical file is still required.
	if !c.IsDirty() {
		t.Fatal("WriteCatalogTo must not clear dirty flag")
	}

	// Adding a second metric and persisting must now land on the canonical
	// file, not the snapshot — i.e. WriteCatalogTo did not rebind c.file.
	if err := c.EnsureMetricEntry("metric.b", 2, Float32Sample); err != nil {
		t.Fatalf("EnsureMetricEntry failed: %v", err)
	}
	if err := c.WriteCatalog(); err != nil {
		t.Fatalf("WriteCatalog failed: %v", err)
	}

	// Snapshot still contains only metric.a; canonical contains both.
	snap, err := LoadCatalog(external)
	if err != nil {
		t.Fatalf("LoadCatalog snapshot failed: %v", err)
	}
	defer snap.Close()
	if _, ok := snap.GetMetricEntry("metric.b"); ok {
		t.Fatal("snapshot must not have been updated by WriteCatalog()")
	}

	canon, err := LoadCatalog(canonical)
	if err != nil {
		t.Fatalf("LoadCatalog canonical failed: %v", err)
	}
	defer canon.Close()
	if _, ok := canon.GetMetricEntry("metric.a"); !ok {
		t.Fatal("canonical missing metric.a")
	}
	if _, ok := canon.GetMetricEntry("metric.b"); !ok {
		t.Fatal("canonical missing metric.b — WriteCatalog() landed on the wrong file")
	}
}

// TestWriteCatalog_ClosedCatalog verifies WriteCatalog returns an error once
// the catalog has been Close()d rather than silently no-op'ing.
func TestWriteCatalog_ClosedCatalog(t *testing.T) {
	dir := t.TempDir()
	c, err := LoadCatalog(filepath.Join(dir, "catalog.json"))
	if err != nil {
		t.Fatalf("LoadCatalog failed: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	if err := c.WriteCatalog(); err == nil {
		t.Fatal("expected WriteCatalog on closed catalog to fail")
	}
}

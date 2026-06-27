package collectors

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseTemperatureMilliDegrees_Empty(t *testing.T) {
	_, err := parseTemperatureMilliDegrees([]byte("\n\t "))
	if err == nil {
		t.Fatalf("expected error")
	}
	if !errors.Is(err, errTemperatureFileEmpty) {
		t.Fatalf("expected errTemperatureFileEmpty, got: %v", err)
	}
	if !strings.Contains(err.Error(), "temperature file is empty") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParseTemperatureMilliDegrees_Invalid(t *testing.T) {
	_, err := parseTemperatureMilliDegrees([]byte("abc"))
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(err.Error(), "temperature file has non-integer value") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestReadOneWireTempMilliDegrees_EmptyTemperatureFile(t *testing.T) {
	root := t.TempDir()
	deviceID := "28-000000000001"
	deviceDir := filepath.Join(root, deviceID)
	if err := os.MkdirAll(deviceDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(deviceDir, "temperature"), []byte(" \n"), 0644); err != nil {
		t.Fatalf("write temperature: %v", err)
	}

	_, err := readOneWireTempMilliDegrees(root, deviceID)
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(err.Error(), "parse temperature file: temperature file is empty") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestOneWireCollector_ShouldLogEmptyRead_RateLimited(t *testing.T) {
	c := NewOneWireCollector(nil, false, "", 0)

	if !c.shouldLogEmptyRead("office_dry") {
		t.Fatalf("first empty read should log")
	}
	for i := 0; i < 58; i++ {
		if c.shouldLogEmptyRead("office_dry") {
			t.Fatalf("unexpected log at miss #%d", i+2)
		}
	}
	if !c.shouldLogEmptyRead("office_dry") {
		t.Fatalf("60th empty read should log")
	}
}

func TestOneWireCollector_ClearEmptyReadMiss_ResetsCadence(t *testing.T) {
	c := NewOneWireCollector(nil, false, "", 0)

	_ = c.shouldLogEmptyRead("office_dry")
	_ = c.shouldLogEmptyRead("office_dry")
	c.clearEmptyReadMiss("office_dry")
	if !c.shouldLogEmptyRead("office_dry") {
		t.Fatalf("first empty read after clear should log")
	}
}

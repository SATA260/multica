package execenv

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPreparePiMcpConfigUsesPiNamespace(t *testing.T) {
	workDir := t.TempDir()
	manifest := &sidecarManifest{}
	raw := json.RawMessage(`{"mcpServers":{"fetch":{"command":"uvx"}}}`)

	if err := preparePiMcpConfig(workDir, "pi", raw, manifest); err != nil {
		t.Fatalf("preparePiMcpConfig: %v", err)
	}
	path := filepath.Join(workDir, ".pi", "mcp.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(data) != string(raw) {
		t.Fatalf("config = %s, want %s", data, raw)
	}
	if !containsPath(manifest.Files, path) || !containsPath(manifest.Dirs, filepath.Dir(path)) {
		t.Fatalf("manifest = %#v, want config file and directory", manifest)
	}
}

func TestPreparePiMcpConfigSkipsEmptyDocument(t *testing.T) {
	workDir := t.TempDir()
	for _, raw := range []json.RawMessage{nil, json.RawMessage("null"), json.RawMessage("  ")} {
		if err := preparePiMcpConfig(workDir, "pi", raw, &sidecarManifest{}); err != nil {
			t.Fatalf("preparePiMcpConfig(%q): %v", raw, err)
		}
	}
	if _, err := os.Stat(filepath.Join(workDir, ".pi", "mcp.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty config created a file, stat error = %v", err)
	}
}

func TestPreparePiMcpConfigRefusesExistingManagedPath(t *testing.T) {
	workDir := t.TempDir()
	path := filepath.Join(workDir, ".pi", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	existing := []byte(`{"mcpServers":{"user":{"command":"echo"}}}`)
	if err := os.WriteFile(path, existing, 0o644); err != nil {
		t.Fatal(err)
	}

	err := preparePiMcpConfig(workDir, "pi", json.RawMessage(`{"mcpServers":{"managed":{}}}`), &sidecarManifest{})
	if err == nil || !strings.Contains(err.Error(), "would overwrite") {
		t.Fatalf("error = %v, want overwrite error", err)
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != string(existing) {
		t.Fatalf("existing config changed to %s", data)
	}
}

func TestPreparePiMcpConfigIgnoresOtherProviders(t *testing.T) {
	workDir := t.TempDir()
	raw := json.RawMessage(`{"mcpServers":{"fetch":{"command":"uvx"}}}`)
	if err := preparePiMcpConfig(workDir, "omp", raw, &sidecarManifest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(workDir, ".pi", "mcp.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pi config written for omp, stat error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, ".omp", "mcp.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("omp config written by pi preparer, stat error = %v", err)
	}
}

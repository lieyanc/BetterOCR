package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMigrateLegacyFile(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "betterocr.json")
	target := filepath.Join(dir, "data", "config.json")
	if err := os.WriteFile(legacy, []byte("legacy"), 0o600); err != nil {
		t.Fatal(err)
	}
	moved, err := migrateLegacyFile(legacy, target)
	if err != nil {
		t.Fatal(err)
	}
	if !moved {
		t.Fatal("legacy file was not migrated")
	}
	raw, err := os.ReadFile(target)
	if err != nil || string(raw) != "legacy" {
		t.Fatalf("target content=%q err=%v", raw, err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("target mode=%o, want 600", info.Mode().Perm())
	}
}

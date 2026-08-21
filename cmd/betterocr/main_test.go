package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/lieyanc/BetterOCR/internal/config"
	"github.com/lieyanc/BetterOCR/internal/database"
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

func TestMigrateLegacyWebSettingsPrefersWebConfiguration(t *testing.T) {
	root := t.TempDir()
	databasePath := filepath.Join(root, "database.json")
	store, err := database.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	var legacyDatabase map[string]json.RawMessage
	if err := json.Unmarshal(raw, &legacyDatabase); err != nil {
		t.Fatal(err)
	}
	legacyConfig := config.Default()
	legacyConfig.EngineTimeoutSeconds = 0
	legacyConfig.ArbiterTimeoutSeconds = 0
	legacyConfig.EngineMaxAttempts = 0
	legacyConfig.ArbiterMaxAttempts = 0
	legacyConfig.TimeoutSeconds = 77
	legacyDatabase["version"] = json.RawMessage("1")
	legacyDatabase["settings"], err = json.Marshal(legacyConfig)
	if err != nil {
		t.Fatal(err)
	}
	raw, err = json.MarshalIndent(legacyDatabase, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(databasePath, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err = database.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}

	configPath := filepath.Join(root, "config.json")
	current := config.Default()
	current.EngineTimeoutSeconds = 10
	if err := config.Save(configPath, current); err != nil {
		t.Fatal(err)
	}
	migratedConfig, migrated, err := migrateLegacyWebSettings(store, configPath, current)
	if err != nil {
		t.Fatal(err)
	}
	want := config.Default()
	want.EngineTimeoutSeconds = 77
	want.ArbiterTimeoutSeconds = 77
	if !migrated || !reflect.DeepEqual(migratedConfig, want) {
		t.Fatalf("migrated=%v config=%+v", migrated, migratedConfig)
	}
	loaded, _, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded, want) {
		t.Fatalf("shared config=%+v want legacy Web config=%+v", loaded, want)
	}
	raw, err = os.ReadFile(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(`"settings"`)) {
		t.Fatalf("settings remain in database: %s", raw)
	}
}

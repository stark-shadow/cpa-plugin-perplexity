package main

import (
	"archive/zip"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

func TestReleaseMetadataAndPackaging(t *testing.T) {
	raw, err := os.ReadFile("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		ID            string   `json:"id"`
		Version       string   `json:"version"`
		Author        string   `json:"author"`
		Repository    string   `json:"repository"`
		SDKVersion    string   `json:"sdk_version"`
		ABIVersion    uint32   `json:"abi_version"`
		SchemaVersion uint32   `json:"schema_version"`
		Platforms     []string `json:"platforms"`
	}
	if json.Unmarshal(raw, &manifest) != nil || manifest.ID != pluginID || manifest.Version != pluginVersion || manifest.Author != "stark-shadow" || manifest.Repository != pluginRepo || manifest.ABIVersion != pluginabi.ABIVersion || manifest.SchemaVersion != pluginabi.SchemaVersion || manifest.SDKVersion != "v8.0.61" || !reflect.DeepEqual(manifest.Platforms, []string{"linux/amd64", "linux/arm64", "windows/amd64", "darwin/amd64", "darwin/arm64"}) {
		t.Fatal("release metadata drift")
	}
	dir := t.TempDir()
	stage := filepath.Join(dir, "stage")
	if err = os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	members := []string{"perplexity.so", "manifest.json", "README.md", "README.zh-CN.md", "LICENSE"}
	for _, name := range members {
		content := []byte("fake library for packaging test")
		if name != "perplexity.so" {
			content, err = os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
		}
		if err = os.WriteFile(filepath.Join(stage, name), content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.WriteFile(filepath.Join(stage, "must-not-package.h"), []byte("extra"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(filepath.Join(stage, "extension"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(stage, "extension", "manifest.json"), []byte(`{"manifest_version":3}`), 0600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "perplexity_1.1.1_linux_amd64.zip")
	if output, runErr := exec.Command("python3", "package.py", stage, target).CombinedOutput(); runErr != nil {
		t.Fatalf("packaging failed: %s", output)
	}
	archive, err := zip.OpenReader(target)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	names := make([]string, 0, len(archive.File))
	for _, file := range archive.File {
		names = append(names, file.Name)
	}
	if !reflect.DeepEqual(names, members) {
		t.Fatalf("ZIP root = %v, want exactly %v", names, members)
	}
	if err = os.Remove(filepath.Join(stage, "LICENSE")); err != nil {
		t.Fatal(err)
	}
	if exec.Command("python3", "package.py", stage, filepath.Join(dir, "bad.zip")).Run() == nil {
		t.Fatal("packager accepted missing release member")
	}
}

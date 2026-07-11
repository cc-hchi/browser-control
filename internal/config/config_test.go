package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDefaultUploadRoots(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(EnvUploadRoots, "")

	cfg, err := Default()
	if err != nil {
		t.Fatalf("Default() error = %v", err)
	}
	want := []string{filepath.Join(home, "Downloads"), filepath.Join(home, "Desktop")}
	if !reflect.DeepEqual(cfg.UploadRoots, want) {
		t.Fatalf("UploadRoots = %v, want %v", cfg.UploadRoots, want)
	}
}

func TestEnvironmentUploadRootsReplaceDefaults(t *testing.T) {
	home := t.TempDir()
	first := filepath.Join(home, "first")
	second := filepath.Join(home, "second")
	t.Setenv("HOME", home)
	t.Setenv(EnvUploadRoots, first+string(os.PathListSeparator)+second)

	cfg, err := Default()
	if err != nil {
		t.Fatalf("Default() error = %v", err)
	}
	want := []string{first, second}
	if !reflect.DeepEqual(cfg.UploadRoots, want) {
		t.Fatalf("UploadRoots = %v, want %v", cfg.UploadRoots, want)
	}
}

func TestEnvironmentNativeManifestOverridesChromeDefault(t *testing.T) {
	home := t.TempDir()
	manifest := filepath.Join(home, "profile", "NativeMessagingHosts", "com.browser_control.native_host.json")
	t.Setenv("HOME", home)
	t.Setenv(EnvNativeHost, manifest)

	cfg, err := Default()
	if err != nil {
		t.Fatalf("Default() error = %v", err)
	}
	if cfg.NativeManifest != manifest {
		t.Fatalf("NativeManifest = %q, want %q", cfg.NativeManifest, manifest)
	}
}

func TestNormalizedUploadRootsRejectsRelativeAndDeduplicates(t *testing.T) {
	root := t.TempDir()
	cfg, err := (Config{UploadRoots: []string{root, filepath.Join(root, ".")}}).Normalized()
	if err != nil {
		t.Fatalf("Normalized() error = %v", err)
	}
	if !reflect.DeepEqual(cfg.UploadRoots, []string{root}) {
		t.Fatalf("UploadRoots = %v, want [%s]", cfg.UploadRoots, root)
	}
	if _, err := (Config{UploadRoots: []string{"relative"}}).Normalized(); err == nil {
		t.Fatal("Normalized() accepted a relative upload root")
	}
}

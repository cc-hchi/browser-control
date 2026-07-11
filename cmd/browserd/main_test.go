package main

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/cc-hchi/browser-control/internal/config"
)

func TestUploadRootFlagReplacesThenAppends(t *testing.T) {
	roots := []string{"/default/Downloads", "/default/Desktop"}
	flag := &uploadRootFlag{target: &roots}
	first := filepath.Join(t.TempDir(), "first")
	second := filepath.Join(t.TempDir(), "second")

	if err := flag.Set(first); err != nil {
		t.Fatalf("first Set() error = %v", err)
	}
	if err := flag.Set(second); err != nil {
		t.Fatalf("second Set() error = %v", err)
	}
	if want := []string{first, second}; !reflect.DeepEqual(roots, want) {
		t.Fatalf("roots = %v, want %v", roots, want)
	}
}

func TestUploadRootFlagRejectsRelativePathWithoutReplacing(t *testing.T) {
	roots := []string{"/default/Downloads"}
	flag := &uploadRootFlag{target: &roots}
	if err := flag.Set("relative"); err == nil {
		t.Fatal("Set() accepted a relative path")
	}
	if want := []string{"/default/Downloads"}; !reflect.DeepEqual(roots, want) {
		t.Fatalf("roots = %v after rejected value, want %v", roots, want)
	}
}

func TestUploadRootFlagCanOverrideInvalidEnvironmentRoots(t *testing.T) {
	roots := []string{"relative-from-environment"}
	flag := &uploadRootFlag{target: &roots}
	valid := t.TempDir()
	if err := flag.Set(valid); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	if _, err := (config.Config{UploadRoots: roots}).Normalized(); err != nil {
		t.Fatalf("valid command-line override did not normalize: %v", err)
	}
}

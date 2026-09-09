package austro_os_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
)

// WalkGoFiles walks the production tree (excluding tests/scripts/vendor) applying fn.
// vendor/ directories are skipped; tracked source remains scanned.
func WalkGoFiles(root string, fn func(path string, data []byte) error) error {
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == ".git" || info.Name() == "node_modules" || info.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if strings.HasPrefix(rel, "tests") || strings.HasPrefix(rel, "scripts") || strings.Contains(rel, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return fn(path, data)
	})
	if err != nil {
		return err
	}
	return nil
}

// WalkAllText walks the whole repo reading only text-like files.
// vendor/ directories are skipped; tracked source is still scanned.
func WalkAllText(root string, fn func(path string, data []byte) error) error {
	exts := map[string]bool{".go": true, ".md": true, ".yaml": true, ".yml": true, ".toml": true, ".sh": true, ".txt": true, ".json": true}
	skip := map[string]bool{".git": true, "node_modules": true, "scripts": true, "tests": true, "vendor": true}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if skip[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !exts[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return fn(path, data)
	})
	if err != nil {
		return err
	}
	return nil
}
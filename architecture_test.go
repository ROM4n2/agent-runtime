package main

import (
	"bytes"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	maxProdLines    = 400
	maxTestLines    = 800
	minScannedFiles = 50
)

var allowedInternalImports = map[string][]string{
	"agent-runtime/config":     {},
	"agent-runtime/metrics":    {},
	"agent-runtime/store":      {},
	"agent-runtime/llm":        {},
	"agent-runtime/prompts":    {},
	"agent-runtime/agent/tool": {"agent-runtime/llm"},
	"agent-runtime/agent":      {"agent-runtime/agent/tool", "agent-runtime/llm"},
	"agent-runtime/retrieval":  {"agent-runtime/agent/tool", "agent-runtime/llm"},
	"agent-runtime/worker":     {"agent-runtime/agent", "agent-runtime/agent/tool", "agent-runtime/llm", "agent-runtime/metrics", "agent-runtime/store"},
	"agent-runtime/api":        {"agent-runtime/agent", "agent-runtime/metrics", "agent-runtime/store"},
	"agent-runtime/eval":       {"agent-runtime/agent", "agent-runtime/agent/tool", "agent-runtime/llm", "agent-runtime/prompts", "agent-runtime/retrieval"},
	"agent-runtime":            {"agent-runtime/agent", "agent-runtime/agent/tool", "agent-runtime/api", "agent-runtime/config", "agent-runtime/eval", "agent-runtime/llm", "agent-runtime/metrics", "agent-runtime/prompts", "agent-runtime/retrieval", "agent-runtime/store", "agent-runtime/worker"},
}

func TestArchitecture_FileLineBudget(t *testing.T) {
	var scannedFiles int
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != "." && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		scannedFiles++

		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("failed to read %s: %v", path, err)
		}
		lines := bytes.Count(data, []byte("\n"))
		if len(data) > 0 && data[len(data)-1] != '\n' {
			lines++
		}

		limit := maxProdLines
		isTest := strings.HasSuffix(path, "_test.go")
		if isTest {
			limit = maxTestLines
		}
		if lines > limit {
			t.Errorf("file line budget exceeded: %s has %d lines (max %d)", filepath.ToSlash(path), lines, limit)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk dir error: %v", err)
	}
	if scannedFiles < minScannedFiles {
		t.Fatalf("scanned file count too low: %d < %d", scannedFiles, minScannedFiles)
	}
}

func TestArchitecture_DAGImports(t *testing.T) {
	fset := token.NewFileSet()
	var scannedFiles int

	allowedMap := make(map[string]map[string]bool)
	for pkg, imps := range allowedInternalImports {
		allowedMap[pkg] = make(map[string]bool)
		for _, imp := range imps {
			allowedMap[pkg][imp] = true
		}
	}

	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != "." && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		scannedFiles++

		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse file %s: %v", path, err)
		}

		if strings.HasSuffix(path, "_test.go") {
			return nil
		}

		relDir := filepath.Dir(path)
		var pkg string
		if relDir == "." || relDir == "" {
			pkg = "agent-runtime"
		} else {
			pkg = "agent-runtime/" + filepath.ToSlash(relDir)
		}

		allowed, ok := allowedMap[pkg]
		if !ok {
			t.Errorf("unregistered package in allowedInternalImports: %s", pkg)
			return nil
		}

		for _, imp := range file.Imports {
			impPath := strings.Trim(imp.Path.Value, `"`)
			if !strings.HasPrefix(impPath, "agent-runtime/") {
				continue
			}
			if !allowed[impPath] {
				t.Errorf("%s illegally imports %s (in %s)", pkg, impPath, filepath.ToSlash(path))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk dir error: %v", err)
	}
	if scannedFiles < minScannedFiles {
		t.Fatalf("scanned file count too low: %d < %d", scannedFiles, minScannedFiles)
	}
}

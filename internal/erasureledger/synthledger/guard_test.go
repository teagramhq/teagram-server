package synthledger_test

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const providerImportPath = "github.com/teagramhq/teagram-server/internal/erasureledger/synthledger"

// TestNoNonTestFileImportsTheProvider keeps the synthetic provider test-only:
// no Go file in the module outside a _test.go file may import it, whatever its
// directory or build constraints.
func TestNoNonTestFileImportsTheProvider(t *testing.T) {
	t.Parallel()
	importers, err := nonTestImporters(filepath.Join("..", "..", ".."), providerImportPath)
	if err != nil {
		t.Fatalf("scan the module: %v", err)
	}
	if len(importers) != 0 {
		t.Fatalf("non-test files import the synthetic provider: %v", importers)
	}
}

// TestImportGuardSeesBuildTaggedFilesUnderTest proves the scan is not limited
// to the default build: a build-tagged non-test file under test/ is reported,
// and a _test.go file is not.
func TestImportGuardSeesBuildTaggedFilesUnderTest(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	importer := fmt.Sprintf("//go:build e2e\n\npackage e2e\n\nimport _ %q\n", providerImportPath)
	for path, body := range map[string]string{
		"go.mod":                      "module example.test\n",
		"test/e2e/tagged.go":          importer,
		"test/e2e/tagged_test.go":     importer,
		"deploy/tool/main.go":         "package main\n\nfunc main() {}\n",
		"internal/other/unrelated.go": "package other\n\nimport _ \"strings\"\n",
	} {
		file := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	importers, err := nonTestImporters(root, providerImportPath)
	if err != nil {
		t.Fatalf("scan the fixture: %v", err)
	}
	if want := []string{"test/e2e/tagged.go"}; !slices.Equal(importers, want) {
		t.Fatalf("importers = %v, want %v", importers, want)
	}
}

// nonTestImporters parses every non-_test.go Go file under root, ignoring build
// constraints, and returns the root-relative paths of those importing path.
func nonTestImporters(root, path string) ([]string, error) {
	fset := token.NewFileSet()
	var importers []string
	err := filepath.WalkDir(root, func(file string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "node_modules":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(file, ".go") || strings.HasSuffix(file, "_test.go") {
			return nil
		}
		parsed, err := parser.ParseFile(fset, file, nil, parser.ImportsOnly)
		if err != nil {
			return fmt.Errorf("parse %s: %w", file, err)
		}
		for _, spec := range parsed.Imports {
			imported, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return fmt.Errorf("%s: import path %s: %w", file, spec.Path.Value, err)
			}
			if imported != path {
				continue
			}
			rel, err := filepath.Rel(root, file)
			if err != nil {
				return err
			}
			importers = append(importers, filepath.ToSlash(rel))
		}
		return nil
	})
	slices.Sort(importers)
	return importers, err
}

package mtproto_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestMarkRequestTailnetForTestingIsTestOnly(t *testing.T) {
	t.Parallel()

	const helper = "MarkRequestTailnetForTesting"
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	var violations []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "bin", "node_modules":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		declarations := map[token.Pos]bool{}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if ok && fn.Name.Name == helper {
				declarations[fn.Name.Pos()] = true
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			id, ok := node.(*ast.Ident)
			if !ok || id.Name != helper || declarations[id.Pos()] {
				return true
			}
			violations = append(violations, fmt.Sprintf("%s:%d references %s", rel, fset.Position(id.Pos()).Line, helper))
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk source tree: %v", err)
	}
	if len(violations) != 0 {
		sort.Strings(violations)
		t.Fatalf("test-only tailnet helper is used by production code:\n\t%s", strings.Join(violations, "\n\t"))
	}
}

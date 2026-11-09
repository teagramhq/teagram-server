package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type callSite struct {
	args   []ast.Expr
	caller *ast.FuncDecl
}

type packageSource struct {
	files     []*ast.File
	functions map[string]*ast.FuncDecl
	values    map[string][]ast.Expr
	calls     map[string][]callSite
}

type resolutionKey struct {
	expr ast.Expr
	fn   *ast.FuncDecl
}

func main() {
	root := flag.String("root", ".", "repository root")
	flag.Parse()

	names, err := environmentNames(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, name := range names {
		fmt.Println(name)
	}
}

func environmentNames(root string) ([]string, error) {
	var names = make(map[string]struct{})
	for _, sourceDir := range []string{"internal/config", "internal/linklanding", "cmd/teagramd"} {
		dir := filepath.Join(root, sourceDir)
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return nil, err
		}

		var paths []string
		err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			base := strings.ToLower(filepath.Base(path))
			if strings.Contains(base, "fixture") || strings.Contains(base, "perf") {
				return nil
			}
			paths = append(paths, path)
			return nil
		})
		if err != nil {
			return nil, err
		}
		sort.Strings(paths)

		fset := token.NewFileSet()
		pkg := &packageSource{
			functions: make(map[string]*ast.FuncDecl),
			values:    make(map[string][]ast.Expr),
			calls:     make(map[string][]callSite),
		}
		for _, path := range paths {
			file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return nil, fmt.Errorf("parse %s: %w", path, err)
			}
			pkg.files = append(pkg.files, file)
			for _, declaration := range file.Decls {
				switch declaration := declaration.(type) {
				case *ast.FuncDecl:
					if declaration.Recv == nil {
						pkg.functions[declaration.Name.Name] = declaration
					}
				case *ast.GenDecl:
					for _, spec := range declaration.Specs {
						values, ok := spec.(*ast.ValueSpec)
						if !ok {
							continue
						}
						for i, name := range values.Names {
							if i < len(values.Values) {
								pkg.values[name.Name] = append(pkg.values[name.Name], values.Values[i])
							}
						}
					}
				}
			}
		}
		pkg.collectCalls()
		pkg.collectEnvironmentReads(names)
	}

	result := make([]string, 0, len(names))
	for name := range names {
		result = append(result, name)
	}
	sort.Strings(result)
	return result, nil
}

func (pkg *packageSource) collectCalls() {
	for _, fn := range pkg.functions {
		if fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			if ident, ok := call.Fun.(*ast.Ident); ok {
				pkg.calls[ident.Name] = append(pkg.calls[ident.Name], callSite{args: call.Args, caller: fn})
			}
			return true
		})
	}
}

func (pkg *packageSource) collectEnvironmentReads(names map[string]struct{}) {
	for _, file := range pkg.files {
		for _, declaration := range file.Decls {
			fn, ok := declaration.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok || len(call.Args) == 0 {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || selector.Sel.Name != "Getenv" && selector.Sel.Name != "LookupEnv" {
					return true
				}
				pkgName, ok := selector.X.(*ast.Ident)
				if !ok || pkgName.Name != "os" {
					return true
				}
				pkg.resolve(call.Args[0], fn, names, make(map[resolutionKey]bool), 0)
				return true
			})
		}
	}
}

func (pkg *packageSource) resolve(expr ast.Expr, fn *ast.FuncDecl, names map[string]struct{}, seen map[resolutionKey]bool, depth int) {
	if expr == nil || depth > 32 {
		return
	}
	key := resolutionKey{expr: expr, fn: fn}
	if seen[key] {
		return
	}
	seen[key] = true
	defer delete(seen, key)

	switch expr := expr.(type) {
	case *ast.BasicLit:
		if expr.Kind != token.STRING {
			return
		}
		value, err := strconv.Unquote(expr.Value)
		if err == nil && isEnvironmentName(value) {
			names[value] = struct{}{}
		}
	case *ast.ParenExpr:
		pkg.resolve(expr.X, fn, names, seen, depth+1)
	case *ast.Ident:
		if fn != nil {
			if rangeExpressions := rangeSources(fn, expr.Name); len(rangeExpressions) > 0 {
				for _, source := range rangeExpressions {
					pkg.resolve(source, fn, names, seen, depth+1)
				}
				return
			}
			if index, ok := parameterIndex(fn, expr.Name); ok {
				for _, site := range pkg.calls[fn.Name.Name] {
					if index < len(site.args) {
						pkg.resolve(site.args[index], site.caller, names, seen, depth+1)
					}
				}
				return
			}
			if definitions := localDefinitions(fn, expr.Name); len(definitions) > 0 {
				for _, definition := range definitions {
					pkg.resolve(definition, fn, names, seen, depth+1)
				}
				return
			}
		}
		for _, value := range pkg.values[expr.Name] {
			pkg.resolve(value, nil, names, seen, depth+1)
		}
	case *ast.SelectorExpr:
		if fn == nil {
			return
		}
		base, ok := expr.X.(*ast.Ident)
		if !ok {
			return
		}
		for _, source := range rangeSources(fn, base.Name) {
			pkg.resolveRangeField(source, expr.Sel.Name, fn, names, seen, depth+1)
		}
	case *ast.CompositeLit:
		for _, element := range expr.Elts {
			if keyed, ok := element.(*ast.KeyValueExpr); ok {
				pkg.resolve(keyed.Value, fn, names, seen, depth+1)
			} else {
				pkg.resolve(element, fn, names, seen, depth+1)
			}
		}
	case *ast.KeyValueExpr:
		pkg.resolve(expr.Value, fn, names, seen, depth+1)
	}
}

func (pkg *packageSource) resolveRangeField(expr ast.Expr, field string, fn *ast.FuncDecl, names map[string]struct{}, seen map[resolutionKey]bool, depth int) {
	if expr == nil || depth > 32 {
		return
	}
	key := resolutionKey{expr: expr, fn: fn}
	if seen[key] {
		return
	}
	seen[key] = true
	defer delete(seen, key)

	switch expr := expr.(type) {
	case *ast.ParenExpr:
		pkg.resolveRangeField(expr.X, field, fn, names, seen, depth+1)
	case *ast.Ident:
		if fn != nil {
			if definitions := localDefinitions(fn, expr.Name); len(definitions) > 0 {
				for _, definition := range definitions {
					pkg.resolveRangeField(definition, field, fn, names, seen, depth+1)
				}
				return
			}
		}
		for _, value := range pkg.values[expr.Name] {
			pkg.resolveRangeField(value, field, nil, names, seen, depth+1)
		}
	case *ast.CompositeLit:
		var structType *ast.StructType
		switch typ := expr.Type.(type) {
		case *ast.ArrayType:
			if elementType, ok := typ.Elt.(*ast.StructType); ok {
				structType = elementType
			}
		case *ast.StructType:
			structType = typ
		}
		fieldIndex := -1
		if structType != nil {
			index := 0
			for _, item := range structType.Fields.List {
				for _, name := range item.Names {
					if name.Name == field {
						fieldIndex = index
					}
					index++
				}
				if len(item.Names) == 0 {
					index++
				}
			}
		}
		for _, element := range expr.Elts {
			row, ok := element.(*ast.CompositeLit)
			if !ok {
				continue
			}
			for index, value := range row.Elts {
				if keyed, ok := value.(*ast.KeyValueExpr); ok {
					if key, ok := keyed.Key.(*ast.Ident); ok && key.Name == field {
						pkg.resolve(keyed.Value, fn, names, seen, depth+1)
					}
					continue
				}
				if index == fieldIndex {
					pkg.resolve(value, fn, names, seen, depth+1)
				}
			}
		}
	}
}

func isEnvironmentName(value string) bool {
	if !strings.HasPrefix(value, "TG_") || strings.HasPrefix(value, "TG_FIXTURE_") || strings.HasPrefix(value, "TG_PERF_") {
		return false
	}
	if len(value) <= 3 {
		return false
	}
	for _, char := range value {
		if char != '_' && (char < 'A' || char > 'Z') && (char < '0' || char > '9') {
			return false
		}
	}
	return true
}

func rangeSources(fn *ast.FuncDecl, name string) []ast.Expr {
	var sources []ast.Expr
	if fn.Body == nil {
		return sources
	}
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		rangeStmt, ok := node.(*ast.RangeStmt)
		if !ok {
			return true
		}
		if ident, ok := rangeStmt.Key.(*ast.Ident); ok && ident.Name == name {
			sources = append(sources, rangeStmt.X)
		}
		if ident, ok := rangeStmt.Value.(*ast.Ident); ok && ident.Name == name {
			sources = append(sources, rangeStmt.X)
		}
		return true
	})
	return sources
}

func parameterIndex(fn *ast.FuncDecl, name string) (int, bool) {
	if fn.Type.Params == nil {
		return 0, false
	}
	index := 0
	for _, field := range fn.Type.Params.List {
		for _, ident := range field.Names {
			if ident.Name == name {
				return index, true
			}
			index++
		}
		if len(field.Names) == 0 {
			index++
		}
	}
	return 0, false
}

func localDefinitions(fn *ast.FuncDecl, name string) []ast.Expr {
	var definitions []ast.Expr
	if fn.Body == nil {
		return definitions
	}
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.ValueSpec:
			for index, ident := range node.Names {
				if ident.Name == name && index < len(node.Values) {
					definitions = append(definitions, node.Values[index])
				}
			}
		case *ast.AssignStmt:
			for index, lhs := range node.Lhs {
				ident, ok := lhs.(*ast.Ident)
				if !ok || ident.Name != name {
					continue
				}
				if index < len(node.Rhs) {
					definitions = append(definitions, node.Rhs[index])
				} else if len(node.Rhs) == 1 {
					definitions = append(definitions, node.Rhs[0])
				}
			}
		}
		return true
	})
	return definitions
}

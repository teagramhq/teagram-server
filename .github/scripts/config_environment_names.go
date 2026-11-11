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

const maxResolutionDepth = 32

type callSite struct {
	args   []ast.Expr
	caller *functionContext
}

type rangeSource struct {
	expr ast.Expr
	key  bool
}

type functionContext struct {
	name    string
	params  *ast.FieldList
	body    *ast.BlockStmt
	literal *ast.FuncLit
}

type packageSource struct {
	files          []*ast.File
	functions      []*functionContext
	functionByNode map[ast.Node]*functionContext
	values         map[string][]ast.Expr
	calls          map[string][]callSite
	literalCalls   map[*ast.FuncLit][]callSite
	fset           *token.FileSet
}

type resolutionKey struct {
	expr ast.Expr
	fn   *functionContext
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
			paths = append(paths, path)
			return nil
		})
		if err != nil {
			return nil, err
		}
		sort.Strings(paths)

		fset := token.NewFileSet()
		pkg := &packageSource{
			functionByNode: make(map[ast.Node]*functionContext),
			values:         make(map[string][]ast.Expr),
			calls:          make(map[string][]callSite),
			literalCalls:   make(map[*ast.FuncLit][]callSite),
			fset:           fset,
		}
		for _, path := range paths {
			file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return nil, fmt.Errorf("parse %s: %w", path, err)
			}
			pkg.files = append(pkg.files, file)
			for _, declaration := range file.Decls {
				values, ok := declaration.(*ast.GenDecl)
				if !ok {
					continue
				}
				for _, spec := range values.Specs {
					valueSpec, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, name := range valueSpec.Names {
						if i < len(valueSpec.Values) {
							pkg.values[name.Name] = append(pkg.values[name.Name], valueSpec.Values[i])
						}
					}
				}
			}
		}
		pkg.collectFunctions()
		if err := pkg.collectCalls(); err != nil {
			return nil, err
		}
		if err := pkg.collectEnvironmentReads(names); err != nil {
			return nil, err
		}
	}

	result := make([]string, 0, len(names))
	for name := range names {
		result = append(result, name)
	}
	sort.Strings(result)
	return result, nil
}

func (pkg *packageSource) collectFunctions() {
	for _, file := range pkg.files {
		literalNames := make(map[*ast.FuncLit]string)
		ast.Inspect(file, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.ValueSpec:
				for i, value := range node.Values {
					if i < len(node.Names) {
						if literal := functionLiteral(value); literal != nil {
							literalNames[literal] = node.Names[i].Name
						}
					}
				}
			case *ast.AssignStmt:
				for i, value := range node.Rhs {
					if i < len(node.Lhs) {
						if name, ok := node.Lhs[i].(*ast.Ident); ok {
							if literal := functionLiteral(value); literal != nil {
								literalNames[literal] = name.Name
							}
						}
					}
				}
			}
			return true
		})

		for _, declaration := range file.Decls {
			if function, ok := declaration.(*ast.FuncDecl); ok && function.Body != nil {
				context := &functionContext{
					name:   function.Name.Name,
					params: function.Type.Params,
					body:   function.Body,
				}
				pkg.functions = append(pkg.functions, context)
				pkg.functionByNode[function] = context
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			literal, ok := node.(*ast.FuncLit)
			if ok {
				context := &functionContext{
					name:    literalNames[literal],
					params:  literal.Type.Params,
					body:    literal.Body,
					literal: literal,
				}
				pkg.functions = append(pkg.functions, context)
				pkg.functionByNode[literal] = context
			}
			return true
		})
	}
}

func functionLiteral(expr ast.Expr) *ast.FuncLit {
	for {
		parenthesized, ok := expr.(*ast.ParenExpr)
		if !ok {
			break
		}
		expr = parenthesized.X
	}
	literal, ok := expr.(*ast.FuncLit)
	if !ok {
		return nil
	}
	return literal
}

func (pkg *packageSource) collectCalls() error {
	for _, file := range pkg.files {
		if err := pkg.walkFile(file, func(node ast.Node, caller *functionContext) error {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return nil
			}
			site := callSite{args: call.Args, caller: caller}
			switch callee := call.Fun.(type) {
			case *ast.Ident:
				pkg.calls[callee.Name] = append(pkg.calls[callee.Name], site)
			case *ast.SelectorExpr:
				pkg.calls[callee.Sel.Name] = append(pkg.calls[callee.Sel.Name], site)
			case *ast.FuncLit:
				pkg.literalCalls[callee] = append(pkg.literalCalls[callee], site)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func (pkg *packageSource) collectEnvironmentReads(names map[string]struct{}) error {
	for _, file := range pkg.files {
		imports := osImportNames(file)
		if err := pkg.walkFile(file, func(node ast.Node, function *functionContext) error {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return nil
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "Getenv" && selector.Sel.Name != "LookupEnv" {
				return nil
			}
			pkgName, ok := selector.X.(*ast.Ident)
			if !ok {
				return nil
			}
			if _, imported := imports[pkgName.Name]; !imported {
				return nil
			}

			position := pkg.fset.Position(call.Args[0].Pos())
			resolved, err := pkg.resolve(call.Args[0], function, names, make(map[resolutionKey]bool), position, 0)
			if err != nil {
				return err
			}
			if !resolved {
				return fmt.Errorf("%s:%d: cannot resolve environment key to a string literal", position.Filename, position.Line)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func osImportNames(file *ast.File) map[string]struct{} {
	imports := make(map[string]struct{})
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil || path != "os" {
			continue
		}
		name := "os"
		if spec.Name != nil {
			name = spec.Name.Name
		}
		if name != "_" && name != "." {
			imports[name] = struct{}{}
		}
	}
	return imports
}

func (pkg *packageSource) walkFile(file *ast.File, visit func(ast.Node, *functionContext) error) error {
	type activeFunction struct {
		depth   int
		context *functionContext
	}
	var functions []activeFunction
	depth := 0
	var visitErr error
	ast.Inspect(file, func(node ast.Node) bool {
		if node == nil {
			if len(functions) > 0 && functions[len(functions)-1].depth == depth {
				functions = functions[:len(functions)-1]
			}
			depth--
			return true
		}
		depth++
		if function, ok := pkg.functionByNode[node]; ok {
			functions = append(functions, activeFunction{depth: depth, context: function})
		}
		var function *functionContext
		if len(functions) > 0 {
			function = functions[len(functions)-1].context
		}
		if visitErr == nil {
			visitErr = visit(node, function)
		}
		return true
	})
	return visitErr
}

func visitFunctionBody(body *ast.BlockStmt, visit func(ast.Node)) {
	if body == nil {
		return
	}
	ast.Inspect(body, func(node ast.Node) bool {
		if node == nil {
			return false
		}
		if node != body {
			switch node.(type) {
			case *ast.FuncDecl, *ast.FuncLit:
				return false
			}
		}
		visit(node)
		return true
	})
}

func (pkg *packageSource) resolve(expr ast.Expr, function *functionContext, names map[string]struct{}, seen map[resolutionKey]bool, origin token.Position, depth int) (bool, error) {
	if expr == nil {
		return false, nil
	}
	if depth >= maxResolutionDepth {
		return false, fmt.Errorf("%s:%d: environment key resolution exceeded depth %d", origin.Filename, origin.Line, maxResolutionDepth)
	}
	key := resolutionKey{expr: expr, fn: function}
	if seen[key] {
		return false, nil
	}
	seen[key] = true
	defer delete(seen, key)

	switch expr := expr.(type) {
	case *ast.BasicLit:
		if expr.Kind != token.STRING {
			return false, nil
		}
		value, err := strconv.Unquote(expr.Value)
		if err != nil {
			return false, fmt.Errorf("%s:%d: cannot parse environment key: %w", origin.Filename, origin.Line, err)
		}
		if isEnvironmentName(value) {
			names[value] = struct{}{}
		}
		return true, nil
	case *ast.ParenExpr:
		return pkg.resolve(expr.X, function, names, seen, origin, depth+1)
	case *ast.Ident:
		if function != nil {
			if sources := rangeSources(function, expr.Name); len(sources) > 0 {
				return pkg.resolveRangeSources(sources, function, names, seen, origin, depth+1)
			}
			if index, ok := parameterIndex(function, expr.Name); ok {
				sites := pkg.callSites(function)
				if len(sites) == 0 {
					return false, nil
				}
				return pkg.resolveCallSites(sites, index, names, seen, origin, depth+1)
			}
			if definitions := localDefinitions(function, expr.Name); len(definitions) > 0 {
				return pkg.resolveAll(definitions, function, names, seen, origin, depth+1)
			}
		}
		if values := pkg.values[expr.Name]; len(values) > 0 {
			return pkg.resolveAll(values, nil, names, seen, origin, depth+1)
		}
	case *ast.SelectorExpr:
		if function == nil {
			return false, nil
		}
		base, ok := expr.X.(*ast.Ident)
		if !ok {
			return false, nil
		}
		sources := rangeSources(function, base.Name)
		if len(sources) == 0 {
			return false, nil
		}
		expressions := make([]ast.Expr, 0, len(sources))
		for _, source := range sources {
			if source.key {
				return false, nil
			}
			expressions = append(expressions, source.expr)
		}
		return pkg.resolveRangeFields(expressions, expr.Sel.Name, function, names, seen, origin, depth+1)
	case *ast.CompositeLit:
		var elements []ast.Expr
		for _, element := range expr.Elts {
			if keyed, ok := element.(*ast.KeyValueExpr); ok {
				elements = append(elements, keyed.Value)
			} else {
				elements = append(elements, element)
			}
		}
		return pkg.resolveAll(elements, function, names, seen, origin, depth+1)
	case *ast.KeyValueExpr:
		return pkg.resolve(expr.Value, function, names, seen, origin, depth+1)
	}
	return false, nil
}

func (pkg *packageSource) resolveAll(expressions []ast.Expr, function *functionContext, names map[string]struct{}, seen map[resolutionKey]bool, origin token.Position, depth int) (bool, error) {
	if len(expressions) == 0 {
		return false, nil
	}
	allResolved := true
	for _, expression := range expressions {
		resolved, err := pkg.resolve(expression, function, names, seen, origin, depth+1)
		if err != nil {
			return false, err
		}
		allResolved = allResolved && resolved
	}
	return allResolved, nil
}

func (pkg *packageSource) resolveRangeSources(sources []rangeSource, function *functionContext, names map[string]struct{}, seen map[resolutionKey]bool, origin token.Position, depth int) (bool, error) {
	if len(sources) == 0 {
		return false, nil
	}
	allResolved := true
	for _, source := range sources {
		var resolved bool
		var err error
		if source.key {
			resolved, err = pkg.resolveMapKeys(source.expr, function, names, seen, origin, depth+1)
		} else {
			resolved, err = pkg.resolve(source.expr, function, names, seen, origin, depth+1)
		}
		if err != nil {
			return false, err
		}
		allResolved = allResolved && resolved
	}
	return allResolved, nil
}

func (pkg *packageSource) resolveMapKeys(expr ast.Expr, function *functionContext, names map[string]struct{}, seen map[resolutionKey]bool, origin token.Position, depth int) (bool, error) {
	if expr == nil {
		return false, nil
	}
	if depth >= maxResolutionDepth {
		return false, fmt.Errorf("%s:%d: environment key resolution exceeded depth %d", origin.Filename, origin.Line, maxResolutionDepth)
	}
	resolution := resolutionKey{expr: expr, fn: function}
	if seen[resolution] {
		return false, nil
	}
	seen[resolution] = true
	defer delete(seen, resolution)

	switch expr := expr.(type) {
	case *ast.ParenExpr:
		return pkg.resolveMapKeys(expr.X, function, names, seen, origin, depth+1)
	case *ast.CompositeLit:
		if _, isMap := expr.Type.(*ast.MapType); !isMap {
			return false, nil
		}
		keys := make([]ast.Expr, 0, len(expr.Elts))
		for _, element := range expr.Elts {
			keyed, ok := element.(*ast.KeyValueExpr)
			if !ok {
				return false, nil
			}
			keys = append(keys, keyed.Key)
		}
		return pkg.resolveAll(keys, function, names, seen, origin, depth+1)
	case *ast.Ident:
		if function != nil {
			if index, ok := parameterIndex(function, expr.Name); ok {
				return pkg.resolveMapKeysCallSites(pkg.callSites(function), index, names, seen, origin, depth+1)
			}
			if definitions := localDefinitions(function, expr.Name); len(definitions) > 0 {
				return pkg.resolveMapKeysAll(definitions, function, names, seen, origin, depth+1)
			}
		}
		if values := pkg.values[expr.Name]; len(values) > 0 {
			return pkg.resolveMapKeysAll(values, nil, names, seen, origin, depth+1)
		}
	}
	return false, nil
}

func (pkg *packageSource) resolveMapKeysAll(expressions []ast.Expr, function *functionContext, names map[string]struct{}, seen map[resolutionKey]bool, origin token.Position, depth int) (bool, error) {
	if len(expressions) == 0 {
		return false, nil
	}
	allResolved := true
	for _, expression := range expressions {
		resolved, err := pkg.resolveMapKeys(expression, function, names, seen, origin, depth+1)
		if err != nil {
			return false, err
		}
		allResolved = allResolved && resolved
	}
	return allResolved, nil
}

func (pkg *packageSource) resolveMapKeysCallSites(sites []callSite, index int, names map[string]struct{}, seen map[resolutionKey]bool, origin token.Position, depth int) (bool, error) {
	if len(sites) == 0 {
		return false, nil
	}
	allResolved := true
	for _, site := range sites {
		if index >= len(site.args) {
			allResolved = false
			continue
		}
		resolved, err := pkg.resolveMapKeys(site.args[index], site.caller, names, seen, origin, depth+1)
		if err != nil {
			return false, err
		}
		allResolved = allResolved && resolved
	}
	return allResolved, nil
}

func (pkg *packageSource) resolveCallSites(sites []callSite, index int, names map[string]struct{}, seen map[resolutionKey]bool, origin token.Position, depth int) (bool, error) {
	if len(sites) == 0 {
		return false, nil
	}
	allResolved := true
	for _, site := range sites {
		if index >= len(site.args) {
			allResolved = false
			continue
		}
		resolved, err := pkg.resolve(site.args[index], site.caller, names, seen, origin, depth+1)
		if err != nil {
			return false, err
		}
		allResolved = allResolved && resolved
	}
	return allResolved, nil
}

func (pkg *packageSource) resolveRangeFields(expressions []ast.Expr, field string, function *functionContext, names map[string]struct{}, seen map[resolutionKey]bool, origin token.Position, depth int) (bool, error) {
	if len(expressions) == 0 {
		return false, nil
	}
	allResolved := true
	for _, expression := range expressions {
		resolved, err := pkg.resolveRangeField(expression, field, function, names, seen, origin, depth+1)
		if err != nil {
			return false, err
		}
		allResolved = allResolved && resolved
	}
	return allResolved, nil
}

func (pkg *packageSource) resolveRangeField(expr ast.Expr, field string, function *functionContext, names map[string]struct{}, seen map[resolutionKey]bool, origin token.Position, depth int) (bool, error) {
	if expr == nil {
		return false, nil
	}
	if depth >= maxResolutionDepth {
		return false, fmt.Errorf("%s:%d: environment key resolution exceeded depth %d", origin.Filename, origin.Line, maxResolutionDepth)
	}
	key := resolutionKey{expr: expr, fn: function}
	if seen[key] {
		return false, nil
	}
	seen[key] = true
	defer delete(seen, key)

	switch expr := expr.(type) {
	case *ast.ParenExpr:
		return pkg.resolveRangeField(expr.X, field, function, names, seen, origin, depth+1)
	case *ast.Ident:
		if function != nil {
			if sources := rangeSources(function, expr.Name); len(sources) > 0 {
				expressions := make([]ast.Expr, 0, len(sources))
				for _, source := range sources {
					if source.key {
						return false, nil
					}
					expressions = append(expressions, source.expr)
				}
				return pkg.resolveRangeFields(expressions, field, function, names, seen, origin, depth+1)
			}
			if index, ok := parameterIndex(function, expr.Name); ok {
				sites := pkg.callSites(function)
				if len(sites) == 0 {
					return false, nil
				}
				allResolved := true
				for _, site := range sites {
					if index >= len(site.args) {
						allResolved = false
						continue
					}
					resolved, err := pkg.resolveRangeField(site.args[index], field, site.caller, names, seen, origin, depth+1)
					if err != nil {
						return false, err
					}
					allResolved = allResolved && resolved
				}
				return allResolved, nil
			}
			if definitions := localDefinitions(function, expr.Name); len(definitions) > 0 {
				return pkg.resolveRangeFields(definitions, field, function, names, seen, origin, depth+1)
			}
		}
		if values := pkg.values[expr.Name]; len(values) > 0 {
			return pkg.resolveRangeFields(values, field, nil, names, seen, origin, depth+1)
		}
	case *ast.CompositeLit:
		return pkg.resolveCompositeField(expr, field, function, names, seen, origin, depth+1)
	}
	return false, nil
}

func (pkg *packageSource) resolveCompositeField(expr *ast.CompositeLit, field string, function *functionContext, names map[string]struct{}, seen map[resolutionKey]bool, origin token.Position, depth int) (bool, error) {
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

	var expressions []ast.Expr
	allRowsResolved := true
	for _, element := range expr.Elts {
		rowExpr := element
		if keyed, ok := element.(*ast.KeyValueExpr); ok {
			rowExpr = keyed.Value
		}
		row, ok := rowExpr.(*ast.CompositeLit)
		if !ok {
			allRowsResolved = false
			continue
		}
		rowResolved := false
		for index, value := range row.Elts {
			if keyed, ok := value.(*ast.KeyValueExpr); ok {
				if key, ok := keyed.Key.(*ast.Ident); ok && key.Name == field {
					expressions = append(expressions, keyed.Value)
					rowResolved = true
				}
				continue
			}
			if index == fieldIndex {
				expressions = append(expressions, value)
				rowResolved = true
			}
		}
		allRowsResolved = allRowsResolved && rowResolved
	}
	if len(expressions) == 0 || !allRowsResolved {
		return false, nil
	}
	return pkg.resolveAll(expressions, function, names, seen, origin, depth+1)
}

func (pkg *packageSource) callSites(function *functionContext) []callSite {
	var sites []callSite
	if function.name != "" {
		sites = append(sites, pkg.calls[function.name]...)
	}
	if function.literal != nil {
		sites = append(sites, pkg.literalCalls[function.literal]...)
	}
	return sites
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

func rangeSources(function *functionContext, name string) []rangeSource {
	var sources []rangeSource
	if function.body == nil {
		return sources
	}
	visitFunctionBody(function.body, func(node ast.Node) {
		rangeStmt, ok := node.(*ast.RangeStmt)
		if !ok {
			return
		}
		if ident, ok := rangeStmt.Key.(*ast.Ident); ok && ident.Name == name {
			sources = append(sources, rangeSource{expr: rangeStmt.X, key: true})
		} else if ident, ok := rangeStmt.Value.(*ast.Ident); ok && ident.Name == name {
			sources = append(sources, rangeSource{expr: rangeStmt.X})
		}
	})
	return sources
}

func parameterIndex(function *functionContext, name string) (int, bool) {
	if function.params == nil {
		return 0, false
	}
	index := 0
	for _, field := range function.params.List {
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

func localDefinitions(function *functionContext, name string) []ast.Expr {
	var definitions []ast.Expr
	if function.body == nil {
		return definitions
	}
	visitFunctionBody(function.body, func(node ast.Node) {
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
	})
	return definitions
}

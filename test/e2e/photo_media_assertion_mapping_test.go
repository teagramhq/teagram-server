package e2e_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestPhotoMediaFatalBranchesHaveUniqueAssertionIDs(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate photo media assertion mapping test")
	}
	sourcePath := filepath.Join(filepath.Dir(testFile), "photo_media_test.go")
	source, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatalf("read photo media source: %v", err)
	}
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, sourcePath, source, 0)
	if err != nil {
		t.Fatalf("parse photo media source: %v", err)
	}

	directID := regexp.MustCompile(`^\[assert:photo-media\.[a-z0-9-]+\] `)
	pairedID := regexp.MustCompile(`^\[assert:%s/photo-media\.[a-z0-9-]+\] `)
	callsiteID := regexp.MustCompile(`^photo-media\.[a-z0-9-]+$`)
	directIDs := map[string]bool{}
	pairedIDs := map[string]bool{}
	callsiteIDs := map[string]bool{}
	fatalCount := 0
	for _, declaration := range parsed.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "testSmokePhotoMedia" {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			literal, ok := node.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(literal.Value)
			if err != nil || !callsiteID.MatchString(value) {
				return true
			}
			if callsiteIDs[value] {
				t.Errorf("caller assertion ID %s is duplicated", value)
			}
			callsiteIDs[value] = true
			return true
		})
	}
	if len(callsiteIDs) == 0 {
		t.Fatal("photo media scenario has no static caller assertion IDs")
	}
	ast.Inspect(parsed, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || !isPhotoMediaFatal(call) {
			return true
		}
		fatalCount++
		if len(call.Args) == 0 {
			t.Errorf("fatal assertion at line %d has no marker", fileSet.Position(call.Pos()).Line)
			return true
		}
		literal, ok := call.Args[0].(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			t.Errorf("fatal assertion at line %d has no static marker format", fileSet.Position(call.Pos()).Line)
			return true
		}
		message, err := strconv.Unquote(literal.Value)
		if err != nil {
			t.Errorf("fatal assertion at line %d has an invalid marker format: %v", fileSet.Position(call.Pos()).Line, err)
			return true
		}
		if directID.MatchString(message) {
			id := strings.SplitN(strings.TrimPrefix(message, "[assert:"), "]", 2)[0]
			if directIDs[id] {
				t.Errorf("direct assertion ID %s is duplicated", id)
			}
			directIDs[id] = true
			return true
		}
		if pairedID.MatchString(message) {
			id := strings.SplitN(strings.TrimPrefix(message, "[assert:%s/"), "]", 2)[0]
			if pairedIDs[id] {
				t.Errorf("paired assertion ID photo-media.%s is duplicated", id)
			}
			pairedIDs[id] = true
			return true
		}
		t.Errorf("fatal assertion at line %d has no photo-media assertion ID", fileSet.Position(call.Pos()).Line)
		return true
	})

	if fatalCount == 0 {
		t.Fatal("photo media scenario has no fatal assertions")
	}
}

func isPhotoMediaFatal(call *ast.CallExpr) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || (selector.Sel.Name != "Fatal" && selector.Sel.Name != "Fatalf") {
		return false
	}
	receiver, ok := selector.X.(*ast.Ident)
	return ok && receiver.Name == "t"
}

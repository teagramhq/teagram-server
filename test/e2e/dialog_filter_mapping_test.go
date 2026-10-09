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

func TestDialogFilterAssertionMapping(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate dialog filter mapping test")
	}
	sourcePath := filepath.Join(filepath.Dir(testFile), "smoke_test.go")
	source, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatalf("read smoke test source: %v", err)
	}
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, sourcePath, source, 0)
	if err != nil {
		t.Fatalf("parse smoke test source: %v", err)
	}

	var scenario *ast.FuncDecl
	for _, declaration := range parsed.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok && function.Name.Name == "testSmokeDialogFilters" {
			scenario = function
			break
		}
	}
	if scenario == nil {
		t.Fatal("testSmokeDialogFilters function is missing")
	}

	want := map[string]assertionMapping{
		"app-config-fetch-error":           {method: "Fatalf", message: "help.getAppConfig: %v"},
		"app-config-object-type":           {method: "Fatalf", message: "app config = %T, want *tg.JSONObject"},
		"app-config-duplicate-enabled":     {method: "Fatal", message: "app config repeats dialog_filters_enabled"},
		"app-config-enabled-type":          {method: "Fatalf", message: "dialog_filters_enabled = %T, want *tg.JSONBool"},
		"app-config-enabled":               {method: "Fatal", message: "dialog_filters_enabled is missing or false"},
		"defaults-initialize-error":        {method: "Fatalf", message: "initialize default folders: %v"},
		"initial-default-count":            {method: "Fatalf", message: "%s folders = %d, want All chats and four defaults"},
		"initial-default-all-type":         {method: "Fatalf", message: "%s first folder = %T, want All chats"},
		"initial-default-order":            {method: "Fatalf", message: "%s folder %d = %#v, want ID %d %s"},
		"repeat-default-count":             {method: "Fatalf", message: "%s folders = %d, want All chats and four defaults"},
		"repeat-default-all-type":          {method: "Fatalf", message: "%s first folder = %T, want All chats"},
		"repeat-default-order":             {method: "Fatalf", message: "%s folder %d = %#v, want ID %d %s"},
		"repeat-default-read-error":        {method: "Fatalf", message: "repeat default folder read: %v"},
		"create-error":                     {method: "Fatalf", message: "create dialog filter: %v"},
		"created-list-error":               {method: "Fatalf", message: "list dialog filters: %v"},
		"created-count-tags":               {method: "Fatalf", message: "filters = %d, tags enabled = %v, want All chats, four defaults and ID 6"},
		"created-value":                    {method: "Fatalf", message: "listed custom filter = %#v, want ID 6 Groups"},
		"edit-error":                       {method: "Fatalf", message: "edit dialog filter: %v"},
		"edited-list-error":                {method: "Fatalf", message: "list edited dialog filters: %v"},
		"edited-value":                     {method: "Fatalf", message: "edited custom filter = %#v, want ID 6 Bots"},
		"other-owner-list-error":           {method: "Fatalf", message: "other owner get dialog filters: %v"},
		"other-owner-count-tags":           {method: "Fatalf", message: "other owner saw %d folders with tags enabled=%v, want All chats and four defaults"},
		"other-owner-all-type":             {method: "Fatalf", message: "other owner's first folder = %T, want All chats"},
		"other-owner-personal":             {method: "Fatalf", message: "other owner's second folder = %#v, want Personal"},
		"reorder-error":                    {method: "Fatalf", message: "reorder dialog filters: %v"},
		"reordered-list-error":             {method: "Fatalf", message: "list reordered dialog filters: %v"},
		"reordered-first":                  {method: "Fatalf", message: "first reordered filter = %#v, want ID 6"},
		"other-session-restart-read-error": {method: "Fatalf", message: "other authorized session get edited dialog filters after restart: %v"},
		"other-session-restart-count":      {method: "Fatalf", message: "other session saw %d filters after restart, want six"},
		"other-session-restart-value":      {method: "Fatalf", message: "persisted edited filter = %#v, want ID 6 Bots"},
		"other-owner-restart-read-error":   {method: "Fatalf", message: "other owner get dialog filters after restart: %v"},
		"other-owner-restart-count":        {method: "Fatalf", message: "other owner saw %d folders after restart, want All chats and four defaults"},
		"other-owner-restart-personal":     {method: "Fatalf", message: "other owner's persisted second folder = %#v, want Personal"},
		"delete-error":                     {method: "Fatalf", message: "delete dialog filter: %v"},
		"deleted-list-error":               {method: "Fatalf", message: "list dialog filters after delete: %v"},
		"deleted-count":                    {method: "Fatalf", message: "filters after delete = %d, want All chats and four defaults"},
		"suggested-list-error":             {method: "Fatalf", message: "get suggested dialog filters: %v"},
		"suggested-count":                  {method: "Fatalf", message: "suggested filters = %d, want none because all four default titles remain"},
	}
	labels := map[string]string{
		"initial-default-count":    "initial",
		"initial-default-all-type": "initial",
		"initial-default-order":    "initial",
		"repeat-default-count":     "repeated",
		"repeat-default-all-type":  "repeated",
		"repeat-default-order":     "repeated",
	}

	got := make(map[string]assertionMapping)
	fatalCount := 0
	ast.Inspect(scenario.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || !isTestingFatal(call) {
			return true
		}
		fatalCount++
		if len(call.Args) == 0 {
			t.Errorf("fatal assertion at line %d has no message", fileSet.Position(call.Pos()).Line)
			return true
		}
		literal, ok := call.Args[0].(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			t.Errorf("fatal assertion at line %d does not use a literal marker", fileSet.Position(call.Pos()).Line)
			return true
		}
		message, err := strconv.Unquote(literal.Value)
		if err != nil {
			t.Errorf("fatal assertion at line %d has an invalid string literal: %v", fileSet.Position(call.Pos()).Line, err)
			return true
		}
		if strings.Count(message, "[assert:") != 1 || !strings.HasPrefix(message, "[assert:dialog-filters.") {
			t.Errorf("fatal assertion at line %d lacks one dialog-filters assertion ID", fileSet.Position(call.Pos()).Line)
			return true
		}
		markerEnd := strings.IndexByte(message, ']')
		if markerEnd < 0 {
			t.Errorf("fatal assertion at line %d has an unterminated assertion ID", fileSet.Position(call.Pos()).Line)
			return true
		}
		id := message[8:markerEnd]
		if !strings.HasPrefix(id, "dialog-filters.") {
			t.Errorf("fatal assertion at line %d has an invalid assertion ID", fileSet.Position(call.Pos()).Line)
			return true
		}
		suffix := strings.TrimPrefix(id, "dialog-filters.")
		if !regexp.MustCompile(`^[a-z0-9-]+$`).MatchString(suffix) {
			t.Errorf("fatal assertion at line %d has an invalid assertion ID", fileSet.Position(call.Pos()).Line)
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			t.Errorf("fatal assertion at line %d is not a selector call", fileSet.Position(call.Pos()).Line)
			return true
		}
		method := selector.Sel.Name
		mapping := assertionMapping{method: method, message: strings.TrimPrefix(message[markerEnd+1:], " ")}
		if previous, duplicate := got[suffix]; duplicate {
			t.Errorf("assertion ID %s is duplicated: %#v and %#v", id, previous, mapping)
			return true
		}
		got[suffix] = mapping
		if label, hasLabel := labels[suffix]; hasLabel {
			if len(call.Args) < 2 {
				t.Errorf("assertion ID %s is missing its folder label", id)
			} else if argument, ok := call.Args[1].(*ast.BasicLit); !ok || argument.Value != strconv.Quote(label) {
				t.Errorf("assertion ID %s is attached to the wrong folder pass", id)
			}
		}
		return true
	})

	if fatalCount != len(want) {
		t.Errorf("testSmokeDialogFilters has %d fatal assertions, want %d", fatalCount, len(want))
	}
	if len(got) != len(want) {
		t.Errorf("testSmokeDialogFilters maps %d fatal assertions, want %d", len(got), len(want))
	}
	for suffix, expected := range want {
		id := "dialog-filters." + suffix
		actual, ok := got[suffix]
		if !ok {
			t.Errorf("assertion mapping %s is missing", id)
			continue
		}
		if actual != expected {
			t.Errorf("assertion mapping %s = %#v, want %#v", id, actual, expected)
		}
	}
}

type assertionMapping struct {
	method  string
	message string
}

func isTestingFatal(call *ast.CallExpr) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || (selector.Sel.Name != "Fatal" && selector.Sel.Name != "Fatalf") {
		return false
	}
	receiver, ok := selector.X.(*ast.Ident)
	return ok && receiver.Name == "t"
}

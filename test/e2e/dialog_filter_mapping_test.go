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
		"dialog-filters.app-config-fetch-error":           {method: "Fatalf", message: "help.getAppConfig: %v"},
		"dialog-filters.app-config-object-type":           {method: "Fatalf", message: "app config = %T, want *tg.JSONObject"},
		"dialog-filters.app-config-duplicate-enabled":     {method: "Fatal", message: "app config repeats dialog_filters_enabled"},
		"dialog-filters.app-config-enabled-type":          {method: "Fatalf", message: "dialog_filters_enabled = %T, want *tg.JSONBool"},
		"dialog-filters.app-config-enabled":               {method: "Fatal", message: "dialog_filters_enabled is missing or false"},
		"dialog-filters.defaults-initialize-error":        {method: "Fatalf", message: "initialize default folders: %v"},
		"dialog-filters.initial-default-count":            {method: "Fatalf", message: "%s folders = %d, want All chats and four defaults"},
		"dialog-filters.initial-default-all-type":         {method: "Fatalf", message: "%s first folder = %T, want All chats"},
		"dialog-filters.initial-default-order":            {method: "Fatalf", message: "%s folder %d = %#v, want ID %d %s"},
		"dialog-filters.repeat-default-count":             {method: "Fatalf", message: "%s folders = %d, want All chats and four defaults"},
		"dialog-filters.repeat-default-all-type":          {method: "Fatalf", message: "%s first folder = %T, want All chats"},
		"dialog-filters.repeat-default-order":             {method: "Fatalf", message: "%s folder %d = %#v, want ID %d %s"},
		"dialog-filters.repeat-default-read-error":        {method: "Fatalf", message: "repeat default folder read: %v"},
		"dialog-filters.create-error":                     {method: "Fatalf", message: "create dialog filter: %v"},
		"dialog-filters.created-list-error":               {method: "Fatalf", message: "list dialog filters: %v"},
		"dialog-filters.created-count-tags":               {method: "Fatalf", message: "filters = %d, tags enabled = %v, want All chats, four defaults and ID 6"},
		"dialog-filters.created-value":                    {method: "Fatalf", message: "listed custom filter = %#v, want ID 6 Groups"},
		"dialog-filters.edit-error":                       {method: "Fatalf", message: "edit dialog filter: %v"},
		"dialog-filters.edited-list-error":                {method: "Fatalf", message: "list edited dialog filters: %v"},
		"dialog-filters.edited-value":                     {method: "Fatalf", message: "edited custom filter = %#v, want ID 6 Bots"},
		"dialog-filters.other-owner-list-error":           {method: "Fatalf", message: "other owner get dialog filters: %v"},
		"dialog-filters.other-owner-count-tags":           {method: "Fatalf", message: "other owner saw %d folders with tags enabled=%v, want All chats and four defaults"},
		"dialog-filters.other-owner-all-type":             {method: "Fatalf", message: "other owner's first folder = %T, want All chats"},
		"dialog-filters.other-owner-personal":             {method: "Fatalf", message: "other owner's second folder = %#v, want Personal"},
		"dialog-filters.reorder-error":                    {method: "Fatalf", message: "reorder dialog filters: %v"},
		"dialog-filters.reordered-list-error":             {method: "Fatalf", message: "list reordered dialog filters: %v"},
		"dialog-filters.reordered-first":                  {method: "Fatalf", message: "first reordered filter = %#v, want ID 6"},
		"dialog-filters.other-session-restart-read-error": {method: "Fatalf", message: "other authorized session get edited dialog filters after restart: %v"},
		"dialog-filters.other-session-restart-count":      {method: "Fatalf", message: "other session saw %d filters after restart, want six"},
		"dialog-filters.other-session-restart-value":      {method: "Fatalf", message: "persisted edited filter = %#v, want ID 6 Bots"},
		"dialog-filters.other-owner-restart-read-error":   {method: "Fatalf", message: "other owner get dialog filters after restart: %v"},
		"dialog-filters.other-owner-restart-count":        {method: "Fatalf", message: "other owner saw %d folders after restart, want All chats and four defaults"},
		"dialog-filters.other-owner-restart-personal":     {method: "Fatalf", message: "other owner's persisted second folder = %#v, want Personal"},
		"dialog-filters.delete-error":                     {method: "Fatalf", message: "delete dialog filter: %v"},
		"dialog-filters.deleted-list-error":               {method: "Fatalf", message: "list dialog filters after delete: %v"},
		"dialog-filters.deleted-count":                    {method: "Fatalf", message: "filters after delete = %d, want All chats and four defaults"},
		"dialog-filters.suggested-list-error":             {method: "Fatalf", message: "get suggested dialog filters: %v"},
		"dialog-filters.suggested-count":                  {method: "Fatalf", message: "suggested filters = %d, want none because all four default titles remain"},
	}
	labels := map[string]string{
		"dialog-filters.initial-default-count":    "initial",
		"dialog-filters.initial-default-all-type": "initial",
		"dialog-filters.initial-default-order":    "initial",
		"dialog-filters.repeat-default-count":     "repeated",
		"dialog-filters.repeat-default-all-type":  "repeated",
		"dialog-filters.repeat-default-order":     "repeated",
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
		if !regexp.MustCompile(`^dialog-filters\.[a-z0-9-]+$`).MatchString(id) {
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
		if previous, duplicate := got[id]; duplicate {
			t.Errorf("assertion ID %s is duplicated: %#v and %#v", id, previous, mapping)
			return true
		}
		got[id] = mapping
		if label, hasLabel := labels[id]; hasLabel {
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
	for id, expected := range want {
		actual, ok := got[id]
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

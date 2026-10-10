package e2e_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const (
	registrationScenarioName       = "username-registration"
	registrationScenarioHelperName = "testSmokeUsernameRegistration"
	registrationBranchTypeName     = "smokeRegistrationBranch"
	registrationMarkerPrefix       = "[assert:"
	registrationAssertionPrefix    = registrationMarkerPrefix + registrationScenarioName + "."

	// registrationForcedBranch is the branch the diagnostics verifier fails on
	// purpose, so a real Go report travels through the unchanged CI sanitizer.
	registrationForcedBranch = "reserved-session-load"

	// registrationDirectAssertion is the one branch attributed where its failure
	// is produced, in the scenario body, with no phase error to route.
	registrationDirectAssertion = registrationScenarioName + ".create-pending-account"
)

// registrationUnattributedAssertions are printed by a phase's default case: a
// failure that reaches the scenario without a branch, such as a connection error
// out of the client callback.
var registrationUnattributedAssertions = map[string]bool{
	registrationScenarioName + ".reserved-signup-unattributed": true,
	registrationScenarioName + ".signup-unattributed":          true,
	registrationScenarioName + ".sign-in-unattributed":         true,
}

// registrationCase is one switch case of the scenario: the branch constants it
// labels, the assertion ID it prints, and whether it sits inside a func literal.
type registrationCase struct {
	labels  []string
	id      string
	closure bool
}

// registrationMarkers is what the scenario function publishes: assertion IDs
// attributed directly in it, IDs found inside a func literal, and its cases.
type registrationMarkers struct {
	direct   []string
	closures []string
	cases    []registrationCase
}

type closureRange struct {
	start token.Pos
	end   token.Pos
}

// registrationSource parses the committed scenario source, which is the surface
// the CI sanitizer resolves assertion IDs against.
func registrationSource(t *testing.T) *ast.File {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "smoke_test.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse smoke_test.go: %v", err)
	}
	return file
}

// registrationFunc returns the named function declaration.
func registrationFunc(t *testing.T, file *ast.File, name string) *ast.FuncDecl {
	t.Helper()
	for _, decl := range file.Decls {
		function, ok := decl.(*ast.FuncDecl)
		if ok && function.Name.Name == name {
			return function
		}
	}
	t.Fatalf("%s not found in smoke_test.go", name)
	return nil
}

// registrationBranchConstants returns every declared registration branch:
// its Go name and the stable ID suffix it is expected to publish.
func registrationBranchConstants(t *testing.T, file *ast.File) map[string]string {
	t.Helper()
	branches := map[string]string{}
	for _, decl := range file.Decls {
		group, ok := decl.(*ast.GenDecl)
		if !ok || group.Tok != token.CONST {
			continue
		}
		for _, spec := range group.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			if typeIdent, ok := value.Type.(*ast.Ident); !ok || typeIdent.Name != registrationBranchTypeName {
				continue
			}
			for index, name := range value.Names {
				if index >= len(value.Values) {
					t.Fatalf("branch %s has no literal value", name.Name)
				}
				literal, ok := value.Values[index].(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					t.Fatalf("branch %s value is not a string literal", name.Name)
				}
				id, err := strconv.Unquote(literal.Value)
				if err != nil {
					t.Fatalf("branch %s value is not a valid literal: %v", name.Name, err)
				}
				if !isRegistrationIDSuffix(id) {
					t.Fatalf("branch %s publishes %q, which is not a scenario ID suffix", name.Name, id)
				}
				branches[name.Name] = id
			}
		}
	}
	if len(branches) < 30 {
		t.Fatalf("registration scenario declares %d failure branches, want at least 30", len(branches))
	}
	return branches
}

// isRegistrationIDSuffix reports whether id is a single assertion-ID
// segment: the sanitizer parses scenario.ID with one dot and no dot inside a
// segment.
func isRegistrationIDSuffix(id string) bool {
	if id == "" || strings.Contains(id, ".") {
		return false
	}
	for _, r := range id {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
		if !ok {
			return false
		}
	}
	return true
}

// registrationAssertionID returns the scenario assertion ID an assertion call
// publishes, or false when the call carries no scenario marker.
func registrationAssertionID(call *ast.CallExpr) (string, bool) {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	receiver, ok := selector.X.(*ast.Ident)
	if !ok || receiver.Name != "t" {
		return "", false
	}
	switch selector.Sel.Name {
	case "Fatal", "Fatalf", "Error", "Errorf":
	default:
		return "", false
	}
	literal, ok := call.Args[0].(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return "", false
	}
	format, err := strconv.Unquote(literal.Value)
	if err != nil || !strings.HasPrefix(format, registrationAssertionPrefix) {
		return "", false
	}
	end := strings.Index(format, "]")
	if end < 0 {
		return "", false
	}
	return format[len(registrationMarkerPrefix):end], true
}

func insideClosure(ranges []closureRange, pos token.Pos) bool {
	for _, r := range ranges {
		if r.start <= pos && pos <= r.end {
			return true
		}
	}
	return false
}

// registrationCaseFor reads one switch case: the branch constants it labels and
// the assertion ID printed directly in it.
func registrationCaseFor(clause *ast.CaseClause) registrationCase {
	out := registrationCase{}
	for _, label := range clause.List {
		if ident, ok := label.(*ast.Ident); ok {
			out.labels = append(out.labels, ident.Name)
		}
	}
	ast.Inspect(clause, func(node ast.Node) bool {
		if _, ok := node.(*ast.FuncLit); ok {
			return false
		}
		if call, ok := node.(*ast.CallExpr); ok {
			if id, ok := registrationAssertionID(call); ok {
				out.id = id
			}
		}
		return true
	})
	return out
}

// registrationMarkersIn collects the assertions of the scenario function and
// marks which of them sit inside a func literal.
func registrationMarkersIn(function *ast.FuncDecl) registrationMarkers {
	var closures []closureRange
	ast.Inspect(function.Body, func(node ast.Node) bool {
		if literal, ok := node.(*ast.FuncLit); ok {
			closures = append(closures, closureRange{literal.Pos(), literal.End()})
		}
		return true
	})
	var out registrationMarkers
	ast.Inspect(function.Body, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.CaseClause:
			c := registrationCaseFor(typed)
			c.closure = insideClosure(closures, typed.Pos())
			out.cases = append(out.cases, c)
		case *ast.CallExpr:
			if id, ok := registrationAssertionID(typed); ok {
				if insideClosure(closures, typed.Pos()) {
					out.closures = append(out.closures, id)
				} else {
					out.direct = append(out.direct, id)
				}
			}
		}
		return true
	})
	return out
}

func sortedIDs(ids []string) []string {
	out := append([]string(nil), ids...)
	slices.Sort(out)
	return out
}

// TestRegistrationAssertionIDMapping pins the complete branch-to-ID mapping of
// the username-registration scenario. Removing a case, routing a branch to
// another ID, renaming a marker, adding a marker with no branch, or moving a
// marker inside a client callback all fail here: the sanitizer resolves a
// published ID only to a report attributed directly to the scenario function,
// so that mapping is what makes the public CI annotation name the branch.
func TestRegistrationAssertionIDMapping(t *testing.T) {
	file := registrationSource(t)
	branches := registrationBranchConstants(t, file)
	if !slices.Contains(sortedIDs(branchesByIDSuffix(branches)), registrationForcedBranch) {
		t.Fatalf("declared branches do not include %s, which the diagnostics verifier fails on purpose", registrationForcedBranch)
	}

	markers := registrationMarkersIn(registrationFunc(t, file, registrationScenarioHelperName))

	for name, suffix := range branches {
		want := registrationScenarioName + "." + suffix
		var routed []registrationCase
		for _, c := range markers.cases {
			if slices.Contains(c.labels, name) {
				routed = append(routed, c)
			}
		}
		if len(routed) != 1 {
			t.Errorf("branch %s is routed by %d switch cases, want exactly 1", name, len(routed))
			continue
		}
		if routed[0].closure {
			t.Errorf("branch %s is attributed inside a func literal, where the sanitizer cannot resolve its report", name)
		}
		if routed[0].id != want {
			t.Errorf("branch %s prints assertion %q, want %q", name, routed[0].id, want)
		}
	}

	for _, c := range markers.cases {
		if len(c.labels) == 0 {
			if !registrationUnattributedAssertions[c.id] {
				t.Errorf("default case prints %q, which is not an unattributed assertion ID", c.id)
			}
			continue
		}
		for _, label := range c.labels {
			if _, declared := branches[label]; !declared {
				t.Errorf("switch case labels %s, which is not a declared registration branch", label)
			}
		}
	}

	want := make([]string, 0, 1+len(branches)+len(registrationUnattributedAssertions))
	want = append(want, registrationDirectAssertion)
	for _, suffix := range branches {
		want = append(want, registrationScenarioName+"."+suffix)
	}
	for id := range registrationUnattributedAssertions {
		want = append(want, id)
	}
	if got := sortedIDs(markers.direct); !slices.Equal(got, sortedIDs(want)) {
		t.Errorf("scenario publishes %d assertion IDs %v, want the %d branch IDs plus the unattributed and direct IDs %v",
			len(got), got, len(branches), sortedIDs(want))
	}
	if len(markers.direct) != len(sortedIDs(markers.direct)) {
		t.Errorf("scenario publishes duplicate assertion IDs: %v", sortedIDs(markers.direct))
	}
	if len(markers.closures) != 0 {
		t.Errorf("scenario markers inside a func literal: %v; only reports attributed to the scenario function resolve",
			markers.closures)
	}
}

// branchesByIDSuffix lists the ID suffixes every declared branch publishes.
func branchesByIDSuffix(branches map[string]string) []string {
	out := make([]string, 0, len(branches))
	for _, suffix := range branches {
		out = append(out, suffix)
	}
	return out
}

// TestRegistrationScenarioBinding pins the shape the sanitizer binds a scenario
// to: TestSmoke runs the scenario ID in a t.Run closure that calls the
// scenario helper with t, and that call is the report Go attributes.
func TestRegistrationScenarioBinding(t *testing.T) {
	file := registrationSource(t)
	bound := false
	ast.Inspect(registrationFunc(t, file, "TestSmoke").Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Run" {
			return true
		}
		id, ok := call.Args[0].(*ast.BasicLit)
		if !ok || id.Kind != token.STRING {
			return true
		}
		scenario, err := strconv.Unquote(id.Value)
		if err != nil || scenario != registrationScenarioName {
			return true
		}
		closure, ok := call.Args[1].(*ast.FuncLit)
		if !ok {
			t.Errorf("scenario %s is not run by a func(t *testing.T) closure", registrationScenarioName)
			return true
		}
		var invocations []token.Pos
		var nested []closureRange
		ast.Inspect(closure.Body, func(inner ast.Node) bool {
			if literal, ok := inner.(*ast.FuncLit); ok {
				nested = append(nested, closureRange{literal.Pos(), literal.End()})
				return true
			}
			innerCall, ok := inner.(*ast.CallExpr)
			if !ok {
				return true
			}
			if name, ok := innerCall.Fun.(*ast.Ident); ok && name.Name == registrationScenarioHelperName {
				invocations = append(invocations, innerCall.Pos())
			}
			return true
		})
		atTopLevel := 0
		for _, pos := range invocations {
			if !insideClosure(nested, pos) {
				atTopLevel++
			}
		}
		if atTopLevel != 1 {
			t.Errorf("scenario %s calls %s at the closure top level %d times, want exactly 1",
				registrationScenarioName, registrationScenarioHelperName, atTopLevel)
		}
		bound = true
		return true
	})
	if !bound {
		t.Fatalf("TestSmoke does not run scenario %s", registrationScenarioName)
	}
}

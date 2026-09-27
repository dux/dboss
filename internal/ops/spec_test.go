package ops

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// TestEveryActionHasSpec keeps the API catalog in step with the Action constants.
func TestEveryActionHasSpec(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "ops.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]bool{}
	ast.Inspect(file, func(node ast.Node) bool {
		spec, ok := node.(*ast.ValueSpec)
		if !ok || len(spec.Names) != 1 || !strings.HasPrefix(spec.Names[0].Name, "Action") || len(spec.Values) != 1 {
			return true
		}
		if literal, ok := spec.Values[0].(*ast.BasicLit); ok {
			name, _ := strconv.Unquote(literal.Value)
			actions[name] = true
		}
		return true
	})
	if len(actions) == 0 {
		t.Fatal("no Action constants found")
	}
	seen := map[string]bool{}
	for _, spec := range Specs() {
		if seen[spec.Name] {
			t.Errorf("duplicate spec %q", spec.Name)
		}
		seen[spec.Name] = true
		if !actions[spec.Name] {
			t.Errorf("spec %q names no Action constant", spec.Name)
		}
		if spec.Group == "" || spec.Summary == "" {
			t.Errorf("spec %q needs a group and a summary", spec.Name)
		}
	}
	for name := range actions {
		if !seen[name] {
			t.Errorf("action %q has no spec in spec.go", name)
		}
	}
}

func TestSpecParamsAreRequestFields(t *testing.T) {
	fields := map[string]bool{}
	kind := reflect.TypeFor[Request]()
	for i := range kind.NumField() {
		fields[strings.Split(kind.Field(i).Tag.Get("json"), ",")[0]] = true
	}
	for _, spec := range Specs() {
		for _, param := range spec.Params {
			if !fields[param.Name] || param.Name == "method" || param.Name == "actor" {
				t.Errorf("%s: param %q is not a Request field a caller may set", spec.Name, param.Name)
			}
			if param.Desc == "" || param.Type == "" {
				t.Errorf("%s: param %q needs a type and a description", spec.Name, param.Name)
			}
		}
	}
}

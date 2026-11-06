package schema_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"
)

func compileSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	raw, err := os.ReadFile("comporta.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	// Refusing every load keeps the schema free of remote references, in CI and
	// in the editors that consume it.
	compiler.UseLoader(offlineLoader{})
	if err := compiler.AddResource("comporta.schema.json", doc); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile("comporta.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

type offlineLoader struct{}

func (offlineLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("remote schema reference is not allowed: %s", url)
}

// toInstance preserves numeric lexemes as json.Number so decimals reach the
// schema with their exact precision.
func toInstance(node *yaml.Node) (any, error) {
	switch node.Kind {
	case yaml.DocumentNode:
		if len(node.Content) == 0 {
			return nil, nil
		}
		return toInstance(node.Content[0])
	case yaml.MappingNode:
		out := make(map[string]any, len(node.Content)/2)
		for i := 0; i+1 < len(node.Content); i += 2 {
			value, err := toInstance(node.Content[i+1])
			if err != nil {
				return nil, err
			}
			out[node.Content[i].Value] = value
		}
		return out, nil
	case yaml.SequenceNode:
		out := make([]any, 0, len(node.Content))
		for _, child := range node.Content {
			value, err := toInstance(child)
			if err != nil {
				return nil, err
			}
			out = append(out, value)
		}
		return out, nil
	case yaml.ScalarNode:
		switch node.Tag {
		case "!!int", "!!float":
			return json.Number(node.Value), nil
		case "!!bool":
			var value bool
			if err := node.Decode(&value); err != nil {
				return nil, err
			}
			return value, nil
		case "!!null":
			return nil, nil
		default:
			return node.Value, nil
		}
	}
	return nil, fmt.Errorf("unsupported YAML node at line %d", node.Line)
}

func loadInstance(t *testing.T, path string) any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	instance, err := toInstance(&doc)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return instance
}

func TestSchemaCompilesOffline(t *testing.T) {
	compileSchema(t)
}

func TestEveryExampleValidatesAgainstSchema(t *testing.T) {
	schema := compileSchema(t)
	examples, err := filepath.Glob("../examples/*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(examples) == 0 {
		t.Fatal("no example files found")
	}
	for _, path := range examples {
		t.Run(filepath.Base(path), func(t *testing.T) {
			if err := schema.Validate(loadInstance(t, path)); err != nil {
				t.Fatalf("example does not match the schema: %v", err)
			}
		})
	}
}

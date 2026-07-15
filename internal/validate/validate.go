// Package validate checks a schedule against ErsatzTV's own JSON schema.
//
// The schema is vendored (schema/sequential-schedule.schema.json) rather than fetched: the CLI must
// work when the server is unreachable, which is exactly when you want to check your work. The
// server revalidates on upload anyway, so a stale copy here cannot let a bad schedule through; it
// can only cost you a round trip.
package validate

import (
	_ "embed"
	"fmt"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
	"gopkg.in/yaml.v3"
)

//go:embed schema/sequential-schedule.schema.json
var schemaJSON []byte

var compiled *jsonschema.Schema

func load() (*jsonschema.Schema, error) {
	if compiled != nil {
		return compiled, nil
	}

	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(schemaJSON)))
	if err != nil {
		return nil, err
	}

	c := jsonschema.NewCompiler()
	if err := c.AddResource("sequential-schedule.schema.json", doc); err != nil {
		return nil, err
	}
	s, err := c.Compile("sequential-schedule.schema.json")
	if err != nil {
		return nil, err
	}
	compiled = s
	return compiled, nil
}

// Schedule returns human-readable problems, or nil when the schedule is valid.
func Schedule(yamlText string) []string {
	s, err := load()
	if err != nil {
		return []string{fmt.Sprintf("could not load schema: %v", err)}
	}

	var doc any
	if err := yaml.Unmarshal([]byte(yamlText), &doc); err != nil {
		return []string{fmt.Sprintf("invalid YAML: %v", err)}
	}

	// YAML gives map[string]any; the JSON Schema evaluator needs JSON-shaped values.
	doc = toJSONValue(doc)

	if err := s.Validate(doc); err != nil {
		var ve *jsonschema.ValidationError
		if ok := asValidationError(err, &ve); ok {
			return flatten(ve)
		}
		return []string{err.Error()}
	}
	return nil
}

func asValidationError(err error, out **jsonschema.ValidationError) bool {
	ve, ok := err.(*jsonschema.ValidationError)
	if ok {
		*out = ve
	}
	return ok
}

// flatten turns the schema's error tree into readable lines.
//
// The content and playout sections are big oneOf unions, so a single bad field reports a failure
// against every branch. Deduplicating by location and message is what turns a wall of noise into
// "/content/0/order: value must be one of chronological, shuffle".
func flatten(ve *jsonschema.ValidationError) []string {
	printer := message.NewPrinter(language.English)

	var msgs []string
	seen := map[string]bool{}

	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			path := "/"
			if len(e.InstanceLocation) > 0 {
				path = "/" + strings.Join(e.InstanceLocation, "/")
			}
			line := fmt.Sprintf("%s: %s", path, e.ErrorKind.LocalizedString(printer))
			if !seen[line] {
				seen[line] = true
				msgs = append(msgs, line)
			}
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)

	sort.Strings(msgs)
	if len(msgs) > 8 {
		msgs = append(msgs[:8], fmt.Sprintf("... and %d more", len(msgs)-8))
	}
	return msgs
}

func toJSONValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = toJSONValue(val)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[fmt.Sprint(k)] = toJSONValue(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = toJSONValue(val)
		}
		return out
	case int:
		return float64(t)
	default:
		return v
	}
}

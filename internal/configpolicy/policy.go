// Package configpolicy checks repository YAML against the current schema and
// requires English configuration comments. Runtime loading separately supports
// legacy external configurations.
package configpolicy

import (
	"bytes"
	"fmt"
	"gopkg.in/yaml.v3"
	"strings"
	"unicode"
)

// Check validates YAML against schema, which must point to a schema type without
// legacy UnmarshalYAML methods, then checks the comment language.
func Check(data []byte, schema any) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(schema); err != nil {
		return err
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return err
	}
	if err := checkSourceKeys(&document); err != nil {
		return err
	}
	return checkEnglishConfigComments(data)
}

// Enforce source keys independently of custom nested schema decoders.
func checkSourceKeys(node *yaml.Node) error {
	if node.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i].Value, node.Content[i+1]
			if (key == "video" || key == "audio") && value.Kind == yaml.MappingNode {
				for j := 0; j+1 < len(value.Content); j += 2 {
					if value.Content[j].Value == "device" {
						return fmt.Errorf("near line %d: use %s.source instead of %s.device", value.Content[j].Line, key, key)
					}
				}
			}
		}
	}
	for _, child := range node.Content {
		if err := checkSourceKeys(child); err != nil {
			return err
		}
	}
	return nil
}

func checkEnglishConfigComments(data []byte) error {
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return err
	}
	var visit func(*yaml.Node) error
	visit = func(node *yaml.Node) error {
		for _, comment := range []string{node.HeadComment, node.LineComment, node.FootComment} {
			if strings.IndexFunc(comment, func(r rune) bool {
				return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul)
			}) >= 0 {
				return fmt.Errorf("near line %d: configuration comments must use English", node.Line)
			}
		}
		for _, child := range node.Content {
			if err := visit(child); err != nil {
				return err
			}
		}
		return nil
	}
	return visit(&document)
}

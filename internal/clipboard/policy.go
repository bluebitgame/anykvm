// Package clipboard defines clipboard policy shared by remote protocols.
package clipboard

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// Direction is a bit mask relative to a remote protocol's client and server.
type Direction int

const (
	Disabled       Direction = 0
	ClientToServer Direction = 1
	ServerToClient Direction = 2
	Bidirectional  Direction = ClientToServer | ServerToClient
)

func (direction Direction) Valid() bool {
	return direction >= Disabled && direction <= Bidirectional
}

// Allows reports whether all requested direction bits are permitted.
func (direction Direction) Allows(flow Direction) bool {
	return direction.Valid() && flow.Valid() && flow != Disabled && direction&flow == flow
}

func (direction *Direction) UnmarshalYAML(node *yaml.Node) error {
	if node.Tag != "!!int" {
		return fmt.Errorf("clipboard directions must be integers from 0 to 3 (line %d)", node.Line)
	}
	var value int
	if err := node.Decode(&value); err != nil {
		return err
	}
	*direction = Direction(value)
	return nil
}

// Options assigns independent directions to clipboard content types.
// Protocol implementations may support only a subset of these types.
type Options struct {
	Text     Direction `yaml:"text"`
	RichText Direction `yaml:"richtext"`
	Images   Direction `yaml:"images"`
	Files    Direction `yaml:"files"`
}

func DefaultOptions() Options {
	return Options{Text: Bidirectional, RichText: Bidirectional, Images: Bidirectional, Files: Bidirectional}
}

func (options Options) Validate() error {
	for _, option := range []struct {
		name      string
		direction Direction
	}{
		{"text", options.Text}, {"richtext", options.RichText},
		{"images", options.Images}, {"files", options.Files},
	} {
		if !option.direction.Valid() {
			return fmt.Errorf("%s must be an integer from 0 to 3", option.name)
		}
	}
	return nil
}

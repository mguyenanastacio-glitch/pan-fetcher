package indexer

import (
	"encoding/json"
	"errors"

	"gopkg.in/yaml.v3"
)

// IndexerDefinition is the top-level YAML structure for a tracker indexer.
type IndexerDefinition struct {
	Site     string       `yaml:"site"`
	ID       string       `yaml:"id"`
	Name     string       `yaml:"name"`
	Type     string       `yaml:"type,omitempty"`
	Language string       `yaml:"language"`
	Links    StrSlice     `yaml:"links"`
	Caps     Capabilities `yaml:"caps"`
	Search   SearchBlock  `yaml:"search"`
	Login    *LoginBlock  `yaml:"login,omitempty"`
	Ratio    *RatioBlock  `yaml:"ratio,omitempty"`
}

// StrSlice handles YAML fields that can be a single string or a string slice.
type StrSlice []string

func (s *StrSlice) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		*s = []string{value.Value}
	case yaml.SequenceNode:
		return value.Decode((*[]string)(s))
	default:
		return errors.New("expected string or sequence")
	}
	return nil
}

func (s StrSlice) MarshalJSON() ([]byte, error) {
	return json.Marshal([]string(s))
}

// Capabilities declares what search modes this indexer supports.
type Capabilities struct {
	CategoryMappings []CategoryMapping  `yaml:"categorymappings"`
	Categories       CatMap             `yaml:"categories"`
	Modes            map[string][]string `yaml:"modes"`
}

// CatMap handles categories as map with numeric keys (Cardigann format).
type CatMap map[string]string

func (c *CatMap) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.MappingNode {
		return errors.New("expected mapping for categories")
	}
	*c = make(map[string]string)
	for i := 0; i < len(value.Content); i += 2 {
		key := value.Content[i].Value
		val := value.Content[i+1].Value
		(*c)[key] = val
	}
	return nil
}

// CategoryMapping maps tracker-specific category IDs to Torznab-style categories.
type CategoryMapping struct {
	ID   string `yaml:"id"`
	Cat  string `yaml:"cat"`
	Desc string `yaml:"desc,omitempty"`
}

// SearchBlock defines how to perform a search on this indexer.
type SearchBlock struct {
	Path       string                `yaml:"path"`
	Paths      []SearchPath          `yaml:"paths"`
	Inputs     StrMap                `yaml:"inputs"`
	Rows       SelectorBlock         `yaml:"rows"`
	Fields     map[string]FieldBlock `yaml:"fields"`
	PreFilters []FilterBlock         `yaml:"prefilters,omitempty"`
}

// StrMap handles map[string]string with integer values auto-converted.
type StrMap map[string]string

func (s *StrMap) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.MappingNode {
		return errors.New("expected mapping")
	}
	*s = make(map[string]string)
	for i := 0; i < len(value.Content); i += 2 {
		key := value.Content[i].Value
		val := value.Content[i+1].Value
		(*s)[key] = val
	}
	return nil
}

// SearchPath defines a URL path pattern.
type SearchPath struct {
	Path       string   `yaml:"path"`
	Categories []string `yaml:"categories,omitempty"`
	Method     string   `yaml:"method,omitempty"`
}

// SelectorBlock defines how to select result rows from a page.
type SelectorBlock struct {
	Selector    string         `yaml:"selector"`
	After       int            `yaml:"after,omitempty"`
	Filters     []FilterBlock  `yaml:"filters,omitempty"`
	DateHeaders *SelectorBlock `yaml:"dateheaders,omitempty"`
}

// FieldBlock defines how to extract a field from a result row.
type FieldBlock struct {
	Selector  string        `yaml:"selector"`
	Text      FlexString    `yaml:"text"`
	Attribute string        `yaml:"attribute,omitempty"`
	Filters   []FilterBlock `yaml:"filters,omitempty"`
	Optional  bool          `yaml:"optional,omitempty"`
	Remove    string        `yaml:"remove,omitempty"`
}

// FlexString handles YAML values that can be string or int.
type FlexString string

func (f *FlexString) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		*f = FlexString(value.Value)
		return nil
	}
	return errors.New("expected scalar")
}

func (f FlexString) String() string { return string(f) }

// FilterBlock handles both name-only (string) and name+args (map) formats.
type FilterBlock struct {
	Name string   `yaml:"name"`
	Args StrSlice `yaml:"args,omitempty"`
}

func (f *FilterBlock) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		f.Name = value.Value
	case yaml.MappingNode:
		type plain FilterBlock
		return value.Decode((*plain)(f))
	default:
		return errors.New("expected string or map for filter")
	}
	return nil
}

// LoginBlock defines how to authenticate with a private tracker.
type LoginBlock struct {
	Path   string       `yaml:"path"`
	Method string       `yaml:"method"`
	Inputs StrMap       `yaml:"inputs"`
	Error  []ErrorBlock `yaml:"error,omitempty"`
	Test   *LoginTest   `yaml:"test,omitempty"`
}

// ErrorBlock defines how to detect login errors.
type ErrorBlock struct {
	Path     string         `yaml:"path"`
	Selector string         `yaml:"selector"`
	Message  *SelectorBlock `yaml:"message,omitempty"`
}

// LoginTest defines how to verify login success.
type LoginTest struct {
	Path     string `yaml:"path"`
	Selector string `yaml:"selector"`
}

// RatioBlock defines how to extract ratio info.
type RatioBlock struct {
	Path      string        `yaml:"path"`
	Selector  string        `yaml:"selector"`
	Attribute string        `yaml:"attribute,omitempty"`
	Filters   []FilterBlock `yaml:"filters,omitempty"`
}

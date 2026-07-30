// Command configschema generates the JSON Schema for ~/.runny/config.yaml from
// the home.Config struct, so editors can autocomplete and validate the file.
// The schema is derived from the struct, never hand-maintained: a
// golden test regenerates and byte-compares the committed config.schema.json, so
// the two cannot drift silently. Run `bazel run //tools/configschema -- -write`
// after changing the config struct.
//
// The schema describes the file's shape and types — the authoring aid. It does
// not replace home.Config.validate(), which stays the authority at load time for
// the semantic rules a flat schema can't (cleanly) express: duration positivity,
// runner-name length, the macOS guest cap. The few constraints that map directly
// (enums, the required keys, the org-XOR-repo target) are enriched in below so
// the editor catches them early too.
package main

import (
	"encoding/json"
	"reflect"

	"github.com/invopop/jsonschema"

	"github.com/bojanrajkovic/runny/internal/home"
)

// durationType is home.Duration's reflect.Type. It (un)marshals as a Go duration
// literal ("90s"), so its schema is a string — not the int64 it is underneath.
var durationType = reflect.TypeOf(home.Duration(0))

// Generate reflects home.Config into a JSON Schema and returns it as indented
// JSON with a trailing newline. Output is deterministic — property order follows
// struct field order, $defs are keyed by type name and marshaled sorted — so a
// golden test can byte-compare it.
func Generate() ([]byte, error) {
	r := &jsonschema.Reflector{
		// The struct carries yaml tags, not json; read those for property names.
		FieldNameTag: "yaml",
		// Nothing is required unless we enrich it below: the loader defaults
		// almost every field, so a bare config is valid.
		RequiredFromJSONSchemaTags: true,
		// Inline the root Config at the top level; nested structs stay in $defs.
		ExpandedStruct: true,
		Mapper: func(t reflect.Type) *jsonschema.Schema {
			if t == durationType {
				return durationSchema()
			}
			return nil
		},
	}
	schema := r.Reflect(&home.Config{})
	enrich(schema)

	b, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// durationSchema is the schema for a home.Duration field: a Go duration string.
// Deliberately no `pattern` — matching time.ParseDuration exactly (a bare "0",
// signs, leading-dot fractionals, both micro signs) is fiddly, and any mismatch
// would flag a VALID config in the editor. The examples convey the format;
// validate() stays the authority on what actually parses (and on positivity).
func durationSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type:        "string",
		Description: `A Go duration literal, e.g. "90s", "2h", "500ms". An omitted or zero value takes the built-in default.`,
		Examples:    []any{"90s", "2h", "500ms"},
	}
}

// enrich folds in the constraints that map cleanly from validate() onto the
// reflected schema: the required keys, the os/ssh_hardening enums, the pool-name
// pattern, and the org-XOR-repo target. Everything else stays validate()'s job.
func enrich(s *jsonschema.Schema) {
	// At least one pool (validate(): "at least one pool is required").
	s.Required = append(s.Required, "pools")
	if pools, ok := s.Properties.Get("pools"); ok {
		minPools := uint64(1)
		pools.MinItems = &minPools
	}

	if pc := def(s, "PoolConfig"); pc != nil {
		// The keys with no default — a config missing them fails validate().
		pc.Required = []string{"name", "os", "image", "github", "target"}
		setEnum(pc, "os", home.OSDarwin, home.OSLinux, home.OSWindows)
		setEnum(pc, "ssh_hardening", string(home.SSHHardeningOff), string(home.SSHHardeningRotate), string(home.SSHHardeningScramble))
		if name, ok := pc.Properties.Get("name"); ok {
			name.Pattern = home.PoolNamePattern
		}
	}
	if gh := def(s, "GitHubConfig"); gh != nil {
		gh.Required = []string{"app_id", "private_key_path"}
	}
	if tc := def(s, "TargetConfig"); tc != nil {
		// Exactly one of: an org alone, or an owner/repo pair — mirroring
		// validate(), which rejects org mixed with any owner/repo (and a lone
		// owner or repo). A bare oneOf on the required keys is not enough: it
		// would still accept {org, owner} via the org branch, so each branch
		// must also forbid the other target's keys.
		tc.OneOf = []*jsonschema.Schema{
			{
				Required: []string{"org"},
				Not: &jsonschema.Schema{AnyOf: []*jsonschema.Schema{
					{Required: []string{"owner"}},
					{Required: []string{"repo"}},
				}},
			},
			{
				Required: []string{"owner", "repo"},
				Not:      &jsonschema.Schema{Required: []string{"org"}},
			},
		}
	}
}

// def returns the named entry from the schema's $defs, or nil.
func def(s *jsonschema.Schema, name string) *jsonschema.Schema {
	if s.Definitions == nil {
		return nil
	}
	return s.Definitions[name]
}

// setEnum constrains a property of def to a fixed set of string values.
func setEnum(def *jsonschema.Schema, prop string, values ...string) {
	p, ok := def.Properties.Get(prop)
	if !ok {
		return
	}
	p.Enum = make([]any, len(values))
	for i, v := range values {
		p.Enum[i] = v
	}
}

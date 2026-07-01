package main

import (
	_ "embed"
	"encoding/json"
	"reflect"
	"testing"
)

// committedSchema is the config.schema.json checked into the repo. The golden
// test below proves it still equals what Generate() produces from home.Config,
// so the shipped schema can never silently drift from the struct.
//
//go:embed config.schema.json
var committedSchema []byte

// The committed schema must byte-match a fresh generation from the struct. If
// this fails after a config change, regenerate it:
//
//	bazel run //tools/configschema -- -write
func TestSchemaGoldenMatchesCommitted(t *testing.T) {
	got, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !reflect.DeepEqual(got, committedSchema) {
		t.Errorf("config.schema.json is stale — regenerate with `bazel run //tools/configschema -- -write`\n"+
			"generated %d bytes, committed %d bytes", len(got), len(committedSchema))
	}
}

// The generator must produce the constraints that make the schema a useful
// authoring aid (and that mirror home.Config.validate()): yaml-named keys, the
// Duration-as-string mapping, the enums, the required keys, the strict-object
// rule. A regression in any of these (e.g. FieldNameTag or the Mapper breaking)
// would otherwise yield a schema that still parses but no longer matches reality.
func TestGeneratedSchemaShape(t *testing.T) {
	var s map[string]any
	if err := json.Unmarshal(committedSchema, &s); err != nil {
		t.Fatalf("committed schema is not valid JSON: %v", err)
	}

	// yaml-named top-level keys (FieldNameTag: "yaml") — not Go field names.
	props := obj(t, s, "properties")
	for _, key := range []string{"pools", "deadlines", "limits", "retention"} {
		if _, ok := props[key]; !ok {
			t.Errorf("top-level property %q missing — yaml field names not used?", key)
		}
	}
	if _, ok := props["Pools"]; ok {
		t.Error("found Go field name \"Pools\" — FieldNameTag is not reading yaml tags")
	}

	// Strict object: mirrors the loader's yaml.Strict() so a typo'd key is flagged.
	if s["additionalProperties"] != false {
		t.Errorf("top-level additionalProperties = %v, want false", s["additionalProperties"])
	}
	// At least one pool.
	if !contains(strs(t, s, "required"), "pools") {
		t.Error("top-level required does not include pools")
	}
	if mi, _ := obj(t, props, "pools")["minItems"].(float64); mi != 1 {
		t.Errorf("pools.minItems = %v, want 1", mi)
	}

	defs := obj(t, s, "$defs")
	pool := obj(t, defs, "PoolConfig")
	poolProps := obj(t, pool, "properties")

	// Duration maps to a string (not the int64 it is underneath), with examples
	// to convey the format. No pattern, deliberately — see durationSchema.
	sshTimeout := obj(t, poolProps, "ssh_timeout")
	if sshTimeout["type"] != "string" {
		t.Errorf("ssh_timeout.type = %v, want string (Duration Mapper)", sshTimeout["type"])
	}
	if ex, ok := sshTimeout["examples"].([]any); !ok || len(ex) == 0 {
		t.Errorf("ssh_timeout missing duration examples: %v", sshTimeout["examples"])
	}

	// Enums mirror validate()'s allowed sets.
	if got := strs(t, obj(t, poolProps, "os"), "enum"); !reflect.DeepEqual(got, []string{"darwin", "linux"}) {
		t.Errorf("os.enum = %v, want [darwin linux]", got)
	}
	if got := strs(t, obj(t, poolProps, "ssh_hardening"), "enum"); !reflect.DeepEqual(got, []string{"off", "rotate", "scramble"}) {
		t.Errorf("ssh_hardening.enum = %v, want [off rotate scramble]", got)
	}
	// Pool name pattern.
	if obj(t, poolProps, "name")["pattern"] == nil {
		t.Error("pool name missing its pattern")
	}
	// Required keys with no default.
	if got := strs(t, pool, "required"); !reflect.DeepEqual(got, []string{"name", "os", "image", "github", "target"}) {
		t.Errorf("PoolConfig.required = %v", got)
	}

	gh := obj(t, defs, "GitHubConfig")
	if got := strs(t, gh, "required"); !reflect.DeepEqual(got, []string{"app_id", "private_key_path"}) {
		t.Errorf("GitHubConfig.required = %v", got)
	}

	// Target is org XOR owner/repo, and the branches are mutually exclusive: a
	// bare oneOf on the required keys would still accept {org, owner} (which
	// validate() rejects), so each branch must forbid the other's keys via not.
	oneOf, ok := obj(t, defs, "TargetConfig")["oneOf"].([]any)
	if !ok || len(oneOf) != 2 {
		t.Fatalf("TargetConfig.oneOf = %v, want two branches (org | owner+repo)", obj(t, defs, "TargetConfig")["oneOf"])
	}
	for i, branch := range oneOf {
		b, _ := branch.(map[string]any)
		if b["not"] == nil {
			t.Errorf("TargetConfig.oneOf[%d] missing a `not` — mixed targets (e.g. {org, owner}) would be wrongly accepted", i)
		}
	}
}

func obj(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	v, ok := m[key].(map[string]any)
	if !ok {
		t.Fatalf("%q is not an object: %T", key, m[key])
	}
	return v
}

func strs(t *testing.T, m map[string]any, key string) []string {
	t.Helper()
	raw, ok := m[key].([]any)
	if !ok {
		t.Fatalf("%q is not an array: %T", key, m[key])
	}
	out := make([]string, len(raw))
	for i, v := range raw {
		out[i], _ = v.(string)
	}
	return out
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

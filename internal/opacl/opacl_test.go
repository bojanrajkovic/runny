package opacl

import "testing"

func TestContainsUID(t *testing.T) {
	ops := []Operator{{UID: 501, User: "alice"}, {UID: 502, User: "bob"}}
	if !ContainsUID(ops, 502) {
		t.Error("ContainsUID(502) = false, want true")
	}
	if ContainsUID(ops, 503) {
		t.Error("ContainsUID(503) = true, want false")
	}
	if ContainsUID(nil, 501) {
		t.Error("ContainsUID on a nil slice = true, want false")
	}
}

func TestContainsUser(t *testing.T) {
	ops := []Operator{{UID: 501, User: "alice"}, {UID: 502, User: "bob"}}
	if !ContainsUser(ops, "bob") {
		t.Error(`ContainsUser("bob") = false, want true`)
	}
	if ContainsUser(ops, "carol") {
		t.Error(`ContainsUser("carol") = true, want false`)
	}
}

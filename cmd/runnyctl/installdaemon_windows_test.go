//go:build windows

package main

import "testing"

func TestRefuseSystemOperator(t *testing.T) {
	cases := map[string]bool{ // operator -> want error
		`NT AUTHORITY\SYSTEM`: true,
		`nt authority\system`: true, // case-insensitive
		`CORP\alice`:          false,
	}
	for op, wantErr := range cases {
		if err := refuseSystemOperator(op); (err != nil) != wantErr {
			t.Errorf("refuseSystemOperator(%q) error = %v, want error = %v", op, err, wantErr)
		}
	}
}

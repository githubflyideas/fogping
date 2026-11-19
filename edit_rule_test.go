package main

import "testing"

// --edit without a login, on an address other than localhost, must refuse to start.
func TestEditNeedsALoginOrLocalhost(t *testing.T) {
	for _, c := range []struct {
		edit, login, local bool
		refused            bool
	}{
		{true, false, false, true}, // the dangerous one: an open, editable console
		{true, true, false, false}, // with a login
		{true, false, true, false}, // bound to 127.0.0.1 only
		{true, true, true, false},
		{false, false, false, false}, // read-only needs nothing
	} {
		err := checkEditAllowed(c.edit, c.login, c.local)
		if (err != nil) != c.refused {
			t.Errorf("edit=%v login=%v localhost=%v: refused=%v, want %v", c.edit, c.login, c.local, err != nil, c.refused)
		}
	}
}

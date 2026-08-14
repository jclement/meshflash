package main

import "testing"

// Aliases must be one-to-one. Two MeshCore slugs folding onto the same
// canonical id would make two different firmware images collide on the same
// (device, role) key, and one would silently overwrite the other.
func TestDeviceAliasesAreInjective(t *testing.T) {
	seen := map[string]string{}
	for from, to := range deviceAliases {
		if prev, dup := seen[to]; dup {
			t.Errorf("%q and %q both alias to %q; folding them would drop one firmware", prev, from, to)
		}
		seen[to] = from
		if from == to {
			t.Errorf("alias %q -> %q is a no-op", from, to)
		}
		if _, chained := deviceAliases[to]; chained {
			t.Errorf("alias %q -> %q points at another alias", from, to)
		}
	}
}

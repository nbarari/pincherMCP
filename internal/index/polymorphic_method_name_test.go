package index

import "testing"

// #465: trace name="String" inbound on pincher-repo returned 30 spurious
// callers because every `.String()` call in the project bound to the
// single project-local Method named String. This is the parallel of #410
// isStdlibReceiver — that filter caught `strings.Index(...)` where the
// receiver itself is stdlib; this filter catches `localVar.String(...)`
// where the receiver name is opaque but the method name is dead-giveaway
// stdlib-interface.

func TestIsPolymorphicInterfaceMethodName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		// fmt.Stringer / error — the original repro
		{"String", true},
		{"Error", true},
		{"GoString", true},
		// io interfaces
		{"Read", true},
		{"Write", true},
		{"Close", true},
		{"Seek", true},
		{"ReadAt", true},
		{"WriteString", true},
		// sync.Locker
		{"Lock", true},
		{"Unlock", true},
		{"RLock", true},
		{"RUnlock", true},
		{"TryLock", true},
		// sort.Interface
		{"Len", true},
		{"Less", true},
		{"Swap", true},
		// http.Handler
		{"ServeHTTP", true},
		// encoding marshalers
		{"MarshalJSON", true},
		{"UnmarshalJSON", true},
		{"MarshalText", true},
		{"UnmarshalBinary", true},
		// time.Time
		{"Now", true},
		{"Add", true},
		{"Before", true},
		{"After", true},
		// context.Context
		{"Done", true},
		{"Err", true},
		{"Value", true},
		// errors.{Is,As,Unwrap}
		{"Is", true},
		{"As", true},
		{"Unwrap", true},

		// #567: Run added — canonical for *exec.Cmd, *http.Server,
		// goroutine pools, lifecycle handlers. Flipped from `false`
		// in v0.21.0 after dogfood surfaced cmd.Run() false-binds.
		{"Run", true},

		// Project-specific names should NOT be filtered.
		{"UpsertProject", false},
		{"BulkUpsertSymbols", false},
		{"Index", false},
		{"Extract", false},
		{"resolveCalls", false},
		// Empty string — defensive.
		{"", false},
	}
	for _, c := range cases {
		if got := isPolymorphicInterfaceMethodName(c.name); got != c.want {
			t.Errorf("isPolymorphicInterfaceMethodName(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

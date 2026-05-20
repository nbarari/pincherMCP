package ast

import "testing"

func TestShouldSkip_Lockfiles(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		// npm/yarn/pnpm/bun
		{"package-lock.json", true},
		{"frontend/package-lock.json", true},
		{"npm-shrinkwrap.json", true},
		{"yarn.lock", true},
		{"pnpm-lock.yaml", true},
		{"bun.lockb", true},
		{"bun.lock", true},

		// Other ecosystems
		{"Cargo.lock", true},
		{"Cargo.LOCK", true}, // case-insensitive
		{"composer.lock", true},
		{"Gemfile.lock", true},
		{"Pipfile.lock", true},
		{"poetry.lock", true},
		{"uv.lock", true},
		{"pdm.lock", true},
		{"mix.lock", true},
		{"pubspec.lock", true},
		{"Podfile.lock", true},
		{"Cartfile.resolved", true},
		{"Package.resolved", true},
		{"flake.lock", true},
		{"go.sum", true},

		// Path with directories — only basename matters
		{"/abs/path/to/package-lock.json", true},
		{"a/b/c/Cargo.lock", true},

		// NOT skipped — legitimate config that uses similar names
		{"package.json", false},
		{"data.json", false},
		{"config.yaml", false},
		{"docker-compose.yaml", false},
		{"Cargo.toml", false},
		{"poetry.toml", false},
		{"go.mod", false},
		{"README.md", false},
		{"main.go", false},

		// Adjacent-but-not-matching names — must not false-positive
		{"package-lock.json.bak", false},
		{"my-package-lock.json", false},
		{"yarn.lock.old", false},
	}
	for _, c := range cases {
		got, reason := ShouldSkip(c.path)
		if got != c.want {
			t.Errorf("ShouldSkip(%q) = (%v, %q), want %v", c.path, got, reason, c.want)
		}
		if got && reason == "" {
			t.Errorf("ShouldSkip(%q) returned skip=true with empty reason", c.path)
		}
		if !got && reason != "" {
			t.Errorf("ShouldSkip(%q) returned skip=false with non-empty reason %q", c.path, reason)
		}
	}
}

func TestShouldSkip_MinifiedBundles(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"app.min.js", true},
		{"vendor.min.mjs", true},
		{"runtime.min.cjs", true},
		{"styles.min.css", true},
		{"component.min.tsx", true},
		{"lib.min.ts", true},
		{"widget.min.jsx", true},
		{"dist/bundle.min.js", true},

		// NOT minified
		{"app.js", false},
		{"min.js", false},          // bare "min.js" is fine; only the .min.X suffix triggers
		{"foo.minimum.js", false},
		{"some.min.json", false},   // .min.json isn't a real minified pattern
		{"main.go", false},
	}
	for _, c := range cases {
		got, reason := ShouldSkip(c.path)
		if got != c.want {
			t.Errorf("ShouldSkip(%q) = (%v, %q), want %v", c.path, got, reason, c.want)
		}
	}
}

func TestShouldSkip_SourceMaps(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"app.js.map", true},
		{"styles.css.map", true},
		{"bundle.map", true},
		{"dist/main.js.map", true},

		// NOT source maps
		{"map.go", false},
		{"foo.txt", false},
		{"sitemap.xml", false},
	}
	for _, c := range cases {
		got, reason := ShouldSkip(c.path)
		if got != c.want {
			t.Errorf("ShouldSkip(%q) = (%v, %q), want %v", c.path, got, reason, c.want)
		}
	}
}

func TestShouldSkip_GeneratedArtifacts(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		// Yarn Plug'n'Play generated artifacts — skipped.
		{".pnp.cjs", true},
		{".pnp.js", true},
		{".pnp.loader.mjs", true},
		{".pnp.data.json", true},
		{"crates/foo/js/.pnp.cjs", true}, // matches by basename in any subdir

		// NOT generated PnP artifacts — real source must still index.
		{"pnp.go", false},          // a file that merely contains "pnp"
		{"app.cjs", false},         // ordinary CommonJS module
		{"config.data.json", false}, // ordinary JSON, not .pnp.data.json
		{"loader.mjs", false},       // ordinary ESM module
	}
	for _, c := range cases {
		got, reason := ShouldSkip(c.path)
		if got != c.want {
			t.Errorf("ShouldSkip(%q) = (%v, %q), want %v", c.path, got, reason, c.want)
		}
	}
}

func TestShouldSkip_ReasonsAreNonEmpty(t *testing.T) {
	// Sample one of each category, confirm reason text exists.
	for _, path := range []string{"package-lock.json", "Cargo.lock", "app.min.js", "bundle.js.map", ".pnp.cjs"} {
		skip, reason := ShouldSkip(path)
		if !skip || reason == "" {
			t.Errorf("ShouldSkip(%q): expected (true, non-empty), got (%v, %q)", path, skip, reason)
		}
	}
}

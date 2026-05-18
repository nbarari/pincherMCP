package ast

import "testing"

// #1461 v0.73 — PHP extractor promotion from 0.70 → 0.85 (stable
// regex tier). Tests pin every shape the promotion covers.

func TestPHP_Interface(t *testing.T) {
	src := []byte(`<?php
interface Logger {
    public function log(string $msg): void;
    public function level(): int;
}

interface CacheInterface extends Logger {
    public function get(string $key): ?string;
}`)
	result := Extract(src, "PHP", "src/Logger.php")
	if result == nil {
		t.Fatal("nil result")
	}
	byName := map[string]ExtractedSymbol{}
	for _, s := range result.Symbols {
		byName[s.Name] = s
	}
	for _, n := range []string{"Logger", "CacheInterface"} {
		s, ok := byName[n]
		if !ok {
			t.Errorf("expected interface %s; got %v", n, phpKeysOf(byName))
		} else if s.Kind != "Interface" {
			t.Errorf("%s.Kind = %q; want Interface", n, s.Kind)
		}
	}
}

func TestPHP_Trait(t *testing.T) {
	src := []byte(`<?php
trait LoggerTrait {
    private $logFile;

    public function log(string $msg): void {
        // ...
    }
}

trait CacheableTrait {
    abstract public function cacheKey(): string;
}`)
	result := Extract(src, "PHP", "src/LoggerTrait.php")
	if result == nil {
		t.Fatal("nil result")
	}
	byName := map[string]ExtractedSymbol{}
	for _, s := range result.Symbols {
		byName[s.Name] = s
	}
	// Trait is modeled via scopeRE (no separate Class symbol emitted)
	// but inner methods still scope to it. Verify the methods got
	// Parent set.
	for _, m := range []string{"log", "cacheKey"} {
		s, ok := byName[m]
		if !ok {
			t.Errorf("expected method %s inside trait; got %v", m, phpKeysOf(byName))
			continue
		}
		if s.Kind != "Method" {
			t.Errorf("%s should be Method (scoped to trait); got %q", m, s.Kind)
		}
	}
}

func TestPHP_Enum(t *testing.T) {
	// PHP 8.1+ enum syntax — first-class type kind.
	src := []byte(`<?php
enum Status {
    case Active;
    case Inactive;
    case Banned;
}

enum Priority: int {
    case Low = 1;
    case Medium = 2;
    case High = 3;
}`)
	result := Extract(src, "PHP", "src/Status.php")
	if result == nil {
		t.Fatal("nil result")
	}
	byName := map[string]ExtractedSymbol{}
	for _, s := range result.Symbols {
		byName[s.Name] = s
	}
	for _, n := range []string{"Status", "Priority"} {
		s, ok := byName[n]
		if !ok {
			t.Errorf("expected enum %s; got %v", n, phpKeysOf(byName))
			continue
		}
		if s.Kind != "Enum" {
			t.Errorf("%s.Kind = %q; want Enum", n, s.Kind)
		}
	}
}

func TestPHP_AttributePrefix(t *testing.T) {
	// PHP 8+ attribute syntax — `#[...]` not C#'s `[...]`.
	src := []byte(`<?php
#[Route("/users", methods: ["GET"])]
class UserController {
    #[Required]
    public function index(): array { return []; }

    #[Authorize(role: "admin")] public function delete(int $id): void {}
}

#[Entity]
#[Table(name: "users")]
class User {}`)
	result := Extract(src, "PHP", "src/UserController.php")
	if result == nil {
		t.Fatal("nil result")
	}
	byName := map[string]ExtractedSymbol{}
	for _, s := range result.Symbols {
		byName[s.Name] = s
	}
	for _, n := range []string{"UserController", "User"} {
		if _, ok := byName[n]; !ok {
			t.Errorf("expected #[Attribute]-prefixed class %s; got %v", n, phpKeysOf(byName))
		}
	}
	for _, m := range []string{"index", "delete"} {
		if _, ok := byName[m]; !ok {
			t.Errorf("expected #[Attribute]-prefixed method %s; got %v", m, phpKeysOf(byName))
		}
	}
}

func TestPHP_FinalAndReadonlyClass(t *testing.T) {
	src := []byte(`<?php
final class Logger {
    public function log(string $msg): void {}
}

readonly class Config {
    public function __construct(public string $name, public int $port) {}
}

final readonly class ImmutableConfig {
    public function __construct(public string $key) {}
}`)
	result := Extract(src, "PHP", "src/Config.php")
	if result == nil {
		t.Fatal("nil result")
	}
	byName := map[string]ExtractedSymbol{}
	for _, s := range result.Symbols {
		byName[s.Name] = s
	}
	for _, n := range []string{"Logger", "Config", "ImmutableConfig"} {
		if _, ok := byName[n]; !ok {
			t.Errorf("expected class %s; got %v", n, phpKeysOf(byName))
		}
	}
}

func TestPHP_AbstractAndFinalMethod(t *testing.T) {
	src := []byte(`<?php
abstract class BaseHandler {
    abstract public function handle(): string;
    final public function describe(): string {
        return "base";
    }
    public static function instance(): static { return new static(); }
}`)
	result := Extract(src, "PHP", "src/BaseHandler.php")
	if result == nil {
		t.Fatal("nil result")
	}
	byName := map[string]ExtractedSymbol{}
	for _, s := range result.Symbols {
		byName[s.Name] = s
	}
	for _, m := range []string{"handle", "describe", "instance"} {
		s, ok := byName[m]
		if !ok {
			t.Errorf("expected method %s; got %v", m, phpKeysOf(byName))
			continue
		}
		if s.Kind != "Method" {
			t.Errorf("%s.Kind = %q; want Method", m, s.Kind)
		}
	}
}

func TestPHP_ExistingClassBehaviour_Regression(t *testing.T) {
	// Pre-#1461 baseline still works.
	src := []byte(`<?php
abstract class Animal {
    protected string $name;
    public function getName(): string { return $this->name; }
}

class Dog extends Animal {
    public function bark(): void {}
}`)
	result := Extract(src, "PHP", "src/Animal.php")
	if result == nil {
		t.Fatal("nil result")
	}
	byName := map[string]ExtractedSymbol{}
	for _, s := range result.Symbols {
		byName[s.Name] = s
	}
	for _, n := range []string{"Animal", "Dog"} {
		s, ok := byName[n]
		if !ok {
			t.Errorf("expected class %s; got %v", n, phpKeysOf(byName))
		} else if s.Kind != "Class" {
			t.Errorf("%s.Kind = %q; want Class", n, s.Kind)
		}
	}
	for _, m := range []string{"getName", "bark"} {
		if _, ok := byName[m]; !ok {
			t.Errorf("expected method %s; got %v", m, phpKeysOf(byName))
		}
	}
}

func TestPHP_ExtractorConfidenceIs085(t *testing.T) {
	if c := RegisteredConfidence("PHP"); c != 0.85 {
		t.Errorf("PHP registry confidence = %v; want 0.85 (#1461 promotion)", c)
	}
}

func phpKeysOf(m map[string]ExtractedSymbol) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

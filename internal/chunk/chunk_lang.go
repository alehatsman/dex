package chunk

import (
	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/bash"
	"github.com/smacker/go-tree-sitter/c"
	"github.com/smacker/go-tree-sitter/cpp"
	"github.com/smacker/go-tree-sitter/csharp"
	"github.com/smacker/go-tree-sitter/elixir"
	"github.com/smacker/go-tree-sitter/golang"
	"github.com/smacker/go-tree-sitter/java"
	"github.com/smacker/go-tree-sitter/javascript"
	"github.com/smacker/go-tree-sitter/kotlin"
	"github.com/smacker/go-tree-sitter/lua"
	"github.com/smacker/go-tree-sitter/php"
	"github.com/smacker/go-tree-sitter/python"
	"github.com/smacker/go-tree-sitter/ruby"
	"github.com/smacker/go-tree-sitter/rust"
	"github.com/smacker/go-tree-sitter/scala"
	"github.com/smacker/go-tree-sitter/swift"
	"github.com/smacker/go-tree-sitter/typescript/tsx"
	"github.com/smacker/go-tree-sitter/typescript/typescript"
)

// langConfig pairs a tree-sitter language with the node kinds we want
// to surface as chunk roots.
type langConfig struct {
	lang  *sitter.Language
	kinds map[string]bool
}

var languages = map[string]langConfig{
	".go": {golang.GetLanguage(), set(
		"function_declaration", "method_declaration", "type_declaration",
	)},
	".py": {python.GetLanguage(), set(
		"function_definition", "class_definition", "decorated_definition",
	)},
	".js":  {javascript.GetLanguage(), jsKinds()},
	".jsx": {javascript.GetLanguage(), jsKinds()},
	".ts":  {typescript.GetLanguage(), tsKinds()},
	// .tsx uses the TSX grammar, not plain TypeScript — the plain grammar
	// mislexes JSX (`<div>` parses as a type-assertion expression), silently
	// dropping JSX-bodied chunk roots (#236, adjacent to the graph fix #232).
	".tsx": {tsx.GetLanguage(), tsKinds()},
	".rs": {rust.GetLanguage(), set(
		"function_item", "struct_item", "enum_item", "impl_item",
		"trait_item", "mod_item",
	)},
	".java": {java.GetLanguage(), set(
		"method_declaration", "class_declaration", "interface_declaration",
		"enum_declaration",
	)},
	".c": {c.GetLanguage(), set(
		"function_definition", "struct_specifier",
	)},
	".h": {c.GetLanguage(), set(
		"function_definition", "struct_specifier", "declaration",
	)},
	".cc":  {cpp.GetLanguage(), cppKinds()},
	".cpp": {cpp.GetLanguage(), cppKinds()},
	".hpp": {cpp.GetLanguage(), cppKinds()},
	".rb": {ruby.GetLanguage(), set(
		"method", "class", "module", "singleton_method",
	)},
	".lua": {lua.GetLanguage(), set(
		"function_declaration_statement", "local_function_declaration_statement",
	)},
	".sh": {bash.GetLanguage(), set(
		"function_definition",
	)},
	".bash": {bash.GetLanguage(), set(
		"function_definition",
	)},
	".zsh": {bash.GetLanguage(), set(
		"function_definition",
	)},
	".cs": {csharp.GetLanguage(), set(
		"namespace_declaration",
		"class_declaration", "interface_declaration",
		"struct_declaration", "enum_declaration",
	)},
	".kt":  {kotlin.GetLanguage(), kotlinKinds()},
	".kts": {kotlin.GetLanguage(), kotlinKinds()},
	".swift": {swift.GetLanguage(), set(
		"class_declaration", // covers class, struct, enum, extension
		"function_declaration",
		"protocol_declaration",
	)},
	".php": {php.GetLanguage(), set(
		"class_declaration", "interface_declaration",
		"function_definition",
	)},
	".scala": {scala.GetLanguage(), set(
		"class_definition", "object_definition", "trait_definition",
		"function_definition",
	)},
	".ex":  {elixir.GetLanguage(), set("call")},
	".exs": {elixir.GetLanguage(), set("call")},
}

// containerMethods maps top-level container node kinds to the method-level
// node kinds found inside them. We walk one level of body/block wrappers
// to reach the actual method nodes (e.g. Python's `block`, Java's
// `class_body`, JS's `class_body`).
var containerMethods = map[string]map[string]bool{
	"class_declaration": {
		"method_definition":    true, // JS/TS
		"method_declaration":   true, // Java/PHP/C#
		"function_declaration": true, // Kotlin/Swift
		"init_declaration":     true, // Swift
	},
	"class_definition": {
		"function_definition": true, // Python/Scala
	},
	"class_specifier": {
		"function_definition": true, // C++
	},
	"impl_item": {
		"function_item": true, // Rust
	},
	"trait_item": {
		"function_item": true, // Rust
	},
	"interface_declaration": {
		"method_declaration": true, // Java/TS/PHP/C#
	},
	"enum_declaration": {
		"method_declaration": true, // Java/C#
	},
	"module": {
		"method":           true, // Ruby
		"singleton_method": true, // Ruby
	},
	// C# — namespace wraps type declarations
	"namespace_declaration": {
		"class_declaration":     true,
		"interface_declaration": true,
		"struct_declaration":    true,
		"enum_declaration":      true,
	},
	// C# structs can contain methods
	"struct_declaration": {
		"method_declaration": true,
	},
	// Kotlin / Swift — object/singleton declarations contain methods
	"object_declaration": {
		"function_declaration": true, // Kotlin
	},
	// Scala — objects and traits contain methods
	"object_definition": {
		"function_definition": true, // Scala
	},
	"trait_definition": {
		"function_declaration": true, // Scala abstract
		"function_definition":  true, // Scala concrete
	},
	// Swift — protocol body contains method declarations
	"protocol_declaration": {
		"protocol_function_declaration": true, // Swift
	},
}

func set(items ...string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, k := range items {
		m[k] = true
	}
	return m
}

func jsKinds() map[string]bool {
	return set(
		"function_declaration",
		"class_declaration",
		"method_definition",
		"lexical_declaration", // top-level const/let with arrow-fn rhs
		"export_statement",
	)
}

func tsKinds() map[string]bool {
	k := jsKinds()
	k["interface_declaration"] = true
	k["type_alias_declaration"] = true
	k["enum_declaration"] = true
	return k
}

func cppKinds() map[string]bool {
	return set(
		"function_definition",
		"struct_specifier",
		"class_specifier",
		"namespace_definition",
	)
}

func kotlinKinds() map[string]bool {
	return set(
		"class_declaration",    // class and interface (both use class_declaration)
		"function_declaration", // top-level and extension functions
		"object_declaration",   // companion object / singleton
	)
}

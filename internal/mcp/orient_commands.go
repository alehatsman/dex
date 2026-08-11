package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/alehatsman/dex/internal/codemap"
)

// commandRoleOrder fixes the display/priority order of orientation commands so
// the rendered "commands" section is byte-stable and leads with the two an agent
// needs first (build, test).
var commandRoleOrder = []string{"build", "test", "lint", "run", "ci"}

// ExtractProjectCommands finds the canonical build/test/lint/run/ci commands for
// a repo by inspecting its task runner, in priority order — mooncake (tasks.yml)
// → Makefile → package.json scripts. It only emits a command whose target
// actually exists in the file (never fabricated); when no task runner is present
// it falls back to the language default (go.mod, Cargo.toml). Returns at most one
// command per role, ordered by commandRoleOrder, for a cache-stable orientation
// render (#134). Best-effort and read-only: any parse error yields no command
// for that source, and an unreadable repo yields none at all.
func ExtractProjectCommands(root string) []codemap.Command {
	byRole := map[string]string{}
	commandsFromTasksYML(root, byRole)
	commandsFromMakefile(root, byRole)
	commandsFromPackageJSON(root, byRole)
	if len(byRole) == 0 {
		commandsFromLanguage(root, byRole)
	}
	out := make([]codemap.Command, 0, len(byRole))
	for _, role := range commandRoleOrder {
		if cmd, ok := byRole[role]; ok {
			out = append(out, codemap.Command{Label: role, Cmd: cmd})
		}
	}
	return out
}

// classifyTarget maps a task/target/script name to the orientation role it
// fills, or "" when it isn't one of the operational commands we surface. Order
// matters: the more specific "ci"/"test"/"lint" checks precede the broader
// build/run buckets.
func classifyTarget(name string) string {
	n := strings.ToLower(name)
	switch {
	case n == "ci" || n == "ci-fast":
		return "ci"
	case strings.Contains(n, "test"):
		return "test"
	case strings.Contains(n, "lint"):
		return "lint"
	case n == "build" || n == "install" || n == "compile":
		return "build"
	case n == "run" || n == "dev" || n == "start" || n == "serve":
		return "run"
	}
	return ""
}

// setCommandIfAbsent records cmd for role only if the role is not already filled,
// so the first (higher-priority) source and the first matching target per role win.
func setCommandIfAbsent(byRole map[string]string, role, cmd string) {
	if role == "" {
		return
	}
	if _, ok := byRole[role]; !ok {
		byRole[role] = cmd
	}
}

// tasksYMLName matches a top-level mooncake task name (exactly two-space indent).
var tasksYMLName = regexp.MustCompile(`^  ([A-Za-z][\w-]*):`)

// commandsFromTasksYML reads mooncake task names under the top-level `tasks:`
// block and maps the operational ones to `mooncake task <name>`.
func commandsFromTasksYML(root string, byRole map[string]string) {
	data, err := os.ReadFile(filepath.Join(root, "tasks.yml"))
	if err != nil {
		return
	}
	inTasks := false
	for _, line := range strings.Split(string(data), "\n") {
		// A non-indented, non-comment line starts a new top-level block; we only
		// harvest names inside `tasks:`.
		if len(line) > 0 && line[0] != ' ' && line[0] != '\t' && line[0] != '#' {
			inTasks = strings.HasPrefix(line, "tasks:")
			continue
		}
		if !inTasks {
			continue
		}
		if m := tasksYMLName.FindStringSubmatch(line); m != nil {
			setCommandIfAbsent(byRole, classifyTarget(m[1]), "mooncake task "+m[1])
		}
	}
}

// makeTarget matches a Makefile rule target at the start of a line.
var makeTarget = regexp.MustCompile(`^([A-Za-z][\w-]*):`)

// commandsFromMakefile reads Makefile rule targets and maps the operational ones
// to `make <target>`.
func commandsFromMakefile(root string, byRole map[string]string) {
	data, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		if m := makeTarget.FindStringSubmatch(line); m != nil {
			setCommandIfAbsent(byRole, classifyTarget(m[1]), "make "+m[1])
		}
	}
}

// commandsFromPackageJSON reads package.json `scripts` and maps the operational
// ones to `npm run <name>`. Script names are visited in sorted order so the
// chosen command per role is deterministic.
func commandsFromPackageJSON(root string, byRole map[string]string) {
	data, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		return
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return
	}
	names := make([]string, 0, len(pkg.Scripts))
	for name := range pkg.Scripts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		setCommandIfAbsent(byRole, classifyTarget(name), "npm run "+name)
	}
}

// commandsFromLanguage is the fallback when no task runner is present: the
// language's own canonical build/test commands, keyed off a manifest file.
func commandsFromLanguage(root string, byRole map[string]string) {
	switch {
	case fileExistsIn(root, "go.mod"):
		byRole["build"] = "go build ./..."
		byRole["test"] = "go test ./..."
	case fileExistsIn(root, "Cargo.toml"):
		byRole["build"] = "cargo build"
		byRole["test"] = "cargo test"
	}
}

func fileExistsIn(root, name string) bool {
	info, err := os.Stat(filepath.Join(root, name))
	return err == nil && !info.IsDir()
}

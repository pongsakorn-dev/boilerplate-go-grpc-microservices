package test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/example/gomicro/internal/testutil"
)

// allowedCommands is the complete set of executables a Taskfile command may invoke.
//
// This is the single biggest cross-platform footgun in the repository, and it is invisible
// to anyone developing on macOS or Linux.
//
// Task runs commands through mvdan.cc/sh, an embedded POSIX shell written in Go. On Unix
// that shell can exec /bin/rm, /usr/bin/sed and so on, so `rm -rf bin` works. On Windows
// those binaries do not exist and mvdan/sh provides no builtins for them -- so the same
// Taskfile line fails with "executable file not found in %PATH%", naming a command the
// contributor has never heard of and cannot install.
//
// go, docker, kubectl and git are the four tools that genuinely exist on every platform.
// Everything filesystem- or text-shaped lives in cmd/devtool, in Go.
var allowedCommands = map[string]bool{
	"go":      true,
	"docker":  true,
	"kubectl": true,
	"git":     true,
}

type taskfile struct {
	Version string            `yaml:"version"`
	Vars    map[string]string `yaml:"vars"`
	Tasks   map[string]struct {
		Desc string `yaml:"desc"`
		Cmds []any  `yaml:"cmds"`
	} `yaml:"tasks"`
}

func loadTaskfile(t *testing.T) taskfile {
	t.Helper()

	b, err := os.ReadFile(filepath.Join(testutil.RepoRoot(t), "Taskfile.yml"))
	if err != nil {
		t.Fatalf("read Taskfile.yml: %v", err)
	}

	var tf taskfile
	if err := yaml.Unmarshal(b, &tf); err != nil {
		t.Fatalf("parse Taskfile.yml: %v", err)
	}
	if len(tf.Tasks) == 0 {
		t.Fatal("Taskfile.yml declares no tasks -- these guards would silently pass forever")
	}
	return tf
}

// TestNoForbiddenShellCommands is the guard that keeps this template usable on Windows.
func TestNoForbiddenShellCommands(t *testing.T) {
	t.Parallel()

	tf := loadTaskfile(t)

	names := make([]string, 0, len(tf.Tasks))
	for name := range tf.Tasks {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		for _, raw := range tf.Tasks[name].Cmds {
			cmd, ok := raw.(string)
			if !ok {
				// A map-form entry is a `task:` reference, which invokes no executable.
				continue
			}
			first := firstToken(cmd)
			if first == "" {
				continue
			}
			if !allowedCommands[first] {
				t.Errorf("task %q runs %q, whose first command is %q.\n\n"+
					"Only %s may be invoked from a Taskfile. Task's embedded shell has no\n"+
					"builtin for anything else on Windows, so this line fails there with an\n"+
					"error naming a command the contributor cannot install.\n"+
					"Put this logic in cmd/devtool instead.",
					name, cmd, first, sortedKeys(allowedCommands))
			}
		}
	}
}

// unquotedPathRE finds a bare argument containing a slash that is not inside double quotes
// and is not a Go package pattern.
var unquotedPathRE = regexp.MustCompile(`(^|\s)(?:-{1,2}[a-zA-Z-]+[= ])?([A-Za-z0-9_.-]+/[A-Za-z0-9_./-]*)`)

// TestFilePathsAreQuoted guards against paths containing spaces.
//
// Verified on the machine this template was written on: GOROOT is "C:\Program Files\Go".
// Windows users routinely have spaces in their home directory and their checkout path too.
// An unquoted argument that expands to a path with a space silently splits into two
// arguments, and the resulting error names neither the task nor the space.
//
// Go package patterns (./..., ./cmd/orderd) are exempt: they are resolved by the go tool
// relative to the module and never contain spaces.
func TestFilePathsAreQuoted(t *testing.T) {
	t.Parallel()

	tf := loadTaskfile(t)

	for name, task := range tf.Tasks {
		for _, raw := range task.Cmds {
			cmd, ok := raw.(string)
			if !ok {
				continue
			}
			// Strip quoted segments; whatever slash-bearing token remains is unquoted.
			stripped := stripQuoted(cmd)

			for _, m := range unquotedPathRE.FindAllStringSubmatch(stripped, -1) {
				path := m[2]
				switch {
				case strings.HasPrefix(path, "./"), strings.HasPrefix(path, "../"):
					continue // Go package pattern
				case strings.Contains(path, "github.com/"):
					continue // module path in `go run pkg@version`
				case strings.Contains(path, "/") && strings.Contains(path, "."):
					t.Errorf("task %q has an unquoted file path %q in:\n    %s\n\n"+
						"Wrap it in double quotes. A path containing a space (GOROOT here is\n"+
						"%q) otherwise splits into two arguments.",
						name, path, cmd, `C:\Program Files\Go`)
				}
			}
		}
	}
}

// TestEveryTaskHasADescription keeps `task --list-all` useful.
//
// An undescribed task is invisible in the task list, so it may as well not exist -- and a
// contributor reinvents whatever it did.
func TestEveryTaskHasADescription(t *testing.T) {
	t.Parallel()

	tf := loadTaskfile(t)

	var missing []string
	for name, task := range tf.Tasks {
		if strings.TrimSpace(task.Desc) == "" {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)

	if len(missing) > 0 {
		t.Errorf("these tasks have no `desc:` and so are invisible in `task --list-all`:\n  %s",
			strings.Join(missing, "\n  "))
	}
}

// taskRefRE matches a task invocation as written in the README.
var taskRefRE = regexp.MustCompile("`task ([a-z][a-z0-9:._-]*)`")

// TestReadmeOnlyReferencesRealTasks stops the documentation from drifting.
//
// A README that tells a newcomer to run a task that was renamed three commits ago is worse
// than no README: it makes them doubt the whole document at exactly the moment they are
// deciding whether to trust the template.
func TestReadmeOnlyReferencesRealTasks(t *testing.T) {
	t.Parallel()

	tf := loadTaskfile(t)

	b, err := os.ReadFile(filepath.Join(testutil.RepoRoot(t), "README.md"))
	if err != nil {
		t.Skipf("no README.md yet: %v", err)
	}

	seen := map[string]bool{}
	for _, m := range taskRefRE.FindAllStringSubmatch(string(b), -1) {
		name := m[1]
		if seen[name] {
			continue
		}
		seen[name] = true

		if _, ok := tf.Tasks[name]; !ok {
			t.Errorf("README.md references `task %s`, which is not defined in Taskfile.yml", name)
		}
	}
	if len(seen) == 0 {
		t.Error("README.md references no tasks at all -- the quickstart is probably wrong")
	}
}

func firstToken(cmd string) string {
	fields := strings.Fields(strings.TrimSpace(cmd))
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// stripQuoted removes double-quoted segments so only unquoted text remains.
func stripQuoted(s string) string {
	var out strings.Builder
	inQuote := false
	for _, r := range s {
		if r == '"' {
			inQuote = !inQuote
			continue
		}
		if !inQuote {
			out.WriteRune(r)
		}
	}
	return out.String()
}

func sortedKeys(m map[string]bool) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

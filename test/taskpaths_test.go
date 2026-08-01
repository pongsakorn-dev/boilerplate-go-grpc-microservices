package test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/example/gomicro/internal/testutil"
)

// pathArgRE finds Go package patterns and repo-relative file paths used as arguments.
var pathArgRE = regexp.MustCompile(`(?:^|\s)"?(\./[A-Za-z0-9_./-]+|deploy/[A-Za-z0-9_./-]+)"?`)

// TestTaskTargetsReferenceRealPaths is the inverse of TestReadmeOnlyReferencesRealTasks, and
// nobody wrote it until five targets had already rotted.
//
// The guard machinery was already half-built: a test proved the README only references real
// tasks. The other direction -- that tasks reference real paths -- went unwritten, and five
// of eighteen targets ended up pointing at cmd/migrate, deploy/ and test/e2e, none of which
// existed. The README tells a reader to run the task list among their first commands, so a
// stranger's second command surfaced five entries failing with
// "package ./cmd/migrate is not in std".
//
// In a repo whose opening claim is "what is not here is listed as not here", a task list
// full of broken entries is the exact overstatement it disclaims. A roadmap belongs in the
// README status table, which is prose and cannot be run. The task list is executable, so
// everything in it must actually work.
func TestTaskTargetsReferenceRealPaths(t *testing.T) {
	t.Parallel()

	tf := loadTaskfile(t)
	root := testutil.RepoRoot(t)

	for name, task := range tf.Tasks {
		for _, raw := range task.Cmds {
			cmd, ok := raw.(string)
			if !ok {
				continue
			}

			for _, m := range pathArgRE.FindAllStringSubmatch(cmd, -1) {
				arg := m[1]

				// Strip Go's "..." wildcard down to the directory it starts from, and the
				// trailing slash from an output path like "bin/".
				dir := strings.TrimSuffix(arg, "...")
				dir = strings.TrimSuffix(dir, "/")
				if dir == "." || dir == "" {
					continue
				}

				// bin/ is created by the build rather than tracked.
				if dir == "./bin" {
					continue
				}

				if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(dir))); err != nil {
					t.Errorf("task %q references %q, which does not exist:\n    %s\n\n"+
						"The task list is executable documentation -- every target in it must run. "+
						"Planned work belongs in the README status table, not here.",
						name, arg, cmd)
				}
			}
		}
	}
}

// Command rename makes this template yours, once.
//
// It rewrites the module path and the container image prefix everywhere they appear,
// regenerates the protobuf code, and then deletes itself -- a fork should not carry a tool
// whose whole job is already done.
//
//	go run ./cmd/rename -module github.com/acme/orders
//
// # Why it regenerates instead of rewriting gen/
//
// This is the reason the tool exists at all, rather than a sed command in the README.
//
// The obvious implementation replaces the module path in every tracked file. Do that and the
// repository still compiles, `go vet` still passes, and then it panics at startup:
//
//	panic: runtime error: slice bounds out of range [-4:]
//	  google.golang.org/protobuf/internal/filedesc.(*File).unmarshalSeed
//	  gen/go/order/v1/order.pb.go:997  file_order_v1_order_proto_init()
//
// Generated .pb.go files embed the serialized FileDescriptorProto as a raw byte string, and
// protobuf serialization is LENGTH-PREFIXED. The descriptor contains go_package, which
// contains the module path. Substituting a path of a different length leaves every following
// prefix pointing at the wrong offset, so the parser walks off the end of the buffer -- at
// init time, before main runs, in a stack trace that names neither the rename nor the module
// path. It was measured, not imagined: "github.com/example/gomicro" is 26 characters and
// "github.com/acme/orders" is 22, and that four-byte difference is the whole bug.
//
// So gen/ is never touched textually. It is REGENERATED, which is correct by construction.
// And if regeneration cannot run -- no module cache, no network -- the tool stops and says so,
// leaving gen/ still importing the old path so the tree fails to COMPILE. A loud failure that
// names the fix beats a silent one that panics in production.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "rename: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		newModule   = flag.String("module", "", "the new Go module path, e.g. github.com/acme/orders")
		imagePrefix = flag.String("image", "", "the new container image prefix (default: the last element of -module)")
		keep        = flag.Bool("keep", false, "do not delete cmd/rename afterwards")
		dryRun      = flag.Bool("dry-run", false, "report what would change and write nothing")
		skipGen     = flag.Bool("skip-codegen", false, "do not regenerate gen/ (the tree will NOT compile until you do)")
	)
	flag.Parse()

	root, err := repoRoot()
	if err != nil {
		return err
	}

	oldModule, err := currentModulePath(root)
	if err != nil {
		return err
	}

	plan, err := newPlan(oldModule, *newModule, *imagePrefix)
	if err != nil {
		return err
	}

	// A DIRTY TREE IS REFUSED, and this is not fussiness.
	//
	// The tool rewrites most files in the repository and then deletes itself. If the change
	// is wrong -- a bad module path, an image prefix that collides -- the only practical
	// undo is `git checkout .`, and that only works if there was nothing else to lose.
	if !*dryRun {
		if err := requireCleanTree(root); err != nil {
			return err
		}
	}

	files, err := trackedFiles(root)
	if err != nil {
		return err
	}

	changed, err := plan.apply(root, files, *dryRun)
	if err != nil {
		return err
	}

	if *dryRun {
		fmt.Printf("would rewrite %d files: %s -> %s\n", len(changed), oldModule, plan.newModule)
		for _, f := range changed {
			fmt.Println("  ", f)
		}
		fmt.Println("would then regenerate gen/ and delete cmd/rename")
		return nil
	}

	fmt.Printf("rewrote %d files: %s -> %s\n", len(changed), oldModule, plan.newModule)
	if plan.oldImage != plan.newImage {
		fmt.Printf("image prefix: %s -> %s\n", plan.oldImage, plan.newImage)
	}

	if *skipGen {
		fmt.Println()
		fmt.Println("gen/ was NOT regenerated, so it still imports the old module path and the")
		fmt.Println("tree will not compile. Run this before anything else:")
		fmt.Println()
		fmt.Println("    go run github.com/bufbuild/buf/cmd/buf@" + bufVersion + " generate")
		return nil
	}

	fmt.Println("regenerating gen/ (this takes a minute the first time)...")
	if err := regenerate(root); err != nil {
		return fmt.Errorf("%w\n\n"+
			"gen/ still imports %s, so the tree will not compile until codegen runs. That is\n"+
			"deliberate: the alternative -- rewriting the generated descriptors as text --\n"+
			"produces a repository that compiles and then panics at init. Fix the cause and run:\n\n"+
			"    go run github.com/bufbuild/buf/cmd/buf@%s generate",
			err, oldModule, bufVersion)
	}

	if !*keep {
		if err := removeSelf(root); err != nil {
			return err
		}
		fmt.Println("deleted cmd/rename and its task target")
	}

	fmt.Println()
	fmt.Println("done. Verify with:")
	fmt.Println("    go build ./... && go test ./...")
	reportWhatWasLeft(plan.oldImage)
	return nil
}

// reportWhatWasLeft names the identity this tool deliberately did NOT rewrite.
//
// The module path is unambiguous: it appears as an import or it does not. The bare word is not.
// It is the compose project name, a database user, a password, a Keycloak realm, a Kubernetes
// namespace, a GORM callback key and a wire header, and those have different blast radii:
//
//   - Compose's project name, database user, password and every DSN must move TOGETHER or the
//     stack fails to authenticate against its own database.
//   - The x- metadata headers are a WIRE CONTRACT. Renaming them breaks interop with any peer
//     still sending the old name, and nothing fails loudly -- the tenant simply arrives empty.
//
// A tool that rewrote some of those and not others would produce a repository that is internally
// inconsistent, which is worse than one that is consistently named after the template. So it
// rewrites none of them and says exactly where they are.
func reportWhatWasLeft(word string) {
	fmt.Printf(`
Still named %q, deliberately -- each set must move together or not at all:

  deploy/compose/docker-compose.yml   project name, database user, password, and every DSN
  deploy/keycloak/                    the realm name, and the issuer URL that names it
  deploy/k8s/                         the namespace and the app.kubernetes.io/part-of label
  internal/platform/client/identity.go   the x-%s-* metadata headers -- a WIRE CONTRACT;
                                         renaming these breaks peers silently
  internal/platform/gormx/            callback names, which are internal to one process
  internal/platform/testdb/           the template database and its credentials

Also yours to change:

  LICENSE      set the copyright holder -- a test fails until you do
  README.md    it describes this project, not yours, including the clone URL
`, word, word)
}

// removeSelf deletes the tool AND everything that referred to it.
//
// DELETING THE DIRECTORY ALONE IS NOT ENOUGH, and the first version of this tool did exactly
// that. The renamed repository built, and then its own test suite failed: `task verify:rename`
// pointed at a package that no longer existed, and the documentation guards found links to
// cmd/rename/main.go. A fork whose tests are red on the first run is worse than one carrying an
// extra tool.
//
// Found by the end-to-end test, which renames a copy of the repository and then runs its tests.
// A test that stopped at "the tool deleted itself" would have called this a success.
//
// The prose references were solved differently -- by not creating them. docs/adr/ points at
// ADR 0004 rather than at this file, precisely because this file is designed to disappear.
func removeSelf(root string) error {
	if err := os.RemoveAll(filepath.Join(root, "cmd", "rename")); err != nil {
		return fmt.Errorf("delete cmd/rename: %w", err)
	}

	taskfile := filepath.Join(root, "Taskfile.yml")
	raw, err := os.ReadFile(taskfile)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// A fork may have removed the Taskfile already. Nothing to clean up.
		return nil
	case err != nil:
		// Anything else -- a permission problem, a directory where a file should be -- is
		// worth reporting rather than swallowing: the tool has already deleted itself, so a
		// silently skipped cleanup leaves a task target pointing at nothing.
		return fmt.Errorf("read Taskfile.yml: %w", err)
	}

	updated, removed := removeTaskTarget(string(raw), "verify:rename")
	if !removed {
		return nil
	}
	if err := os.WriteFile(taskfile, []byte(updated), 0o644); err != nil {
		return fmt.Errorf("rewrite Taskfile.yml: %w", err)
	}
	return nil
}

// removeTaskTarget drops one target block from a Taskfile.
//
// A target runs from its own "  name:" line to the next line at the same indentation, so this
// is a small, exact edit rather than YAML round-tripping -- which would reformat the whole file
// and destroy every comment in it, and this Taskfile is mostly comments.
func removeTaskTarget(content, name string) (string, bool) {
	lines := strings.Split(content, "\n")

	start := -1
	for i, line := range lines {
		if strings.TrimRight(line, " \t\r") == "  "+name+":" {
			start = i
			break
		}
	}
	if start < 0 {
		return content, false
	}

	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" || strings.HasPrefix(lines[i], "    ") || strings.HasPrefix(lines[i], "\t") {
			continue
		}
		end = i
		break
	}

	// Take the blank line before the target too, so removals do not accumulate gaps.
	if start > 0 && strings.TrimSpace(lines[start-1]) == "" {
		start--
	}

	return strings.Join(append(lines[:start:start], lines[end:]...), "\n"), true
}

// bufVersion must match Taskfile.yml, .github/workflows/ci.yml and tools/codegen_uptodate_test.go.
// test/toolpins_test.go asserts every copy is identical.
const bufVersion = "v1.72.0"

// plan is the set of substitutions to make.
type plan struct {
	oldModule, newModule string
	oldImage, newImage   string
}

// modulePathRE is deliberately strict.
//
// A module path with a space, a backslash or a trailing slash produces a go.mod Go refuses to
// read, and by then the tool has rewritten ninety files and deleted itself.
var modulePathRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._~/-]*[a-zA-Z0-9]$`)

func newPlan(oldModule, newModule, newImage string) (plan, error) {
	newModule = strings.TrimSpace(newModule)
	if newModule == "" {
		return plan{}, errors.New("-module is required, e.g. -module github.com/acme/orders")
	}
	if !modulePathRE.MatchString(newModule) {
		return plan{}, fmt.Errorf("%q is not a usable module path", newModule)
	}
	if newModule == oldModule {
		return plan{}, fmt.Errorf("the module path is already %s", oldModule)
	}
	if !strings.Contains(newModule, "/") {
		return plan{}, fmt.Errorf("%q has no host: a module path outside a repository host "+
			"cannot be fetched by anyone else", newModule)
	}

	oldImage := lastElement(oldModule)
	if newImage == "" {
		newImage = lastElement(newModule)
	}
	if strings.ContainsAny(newImage, " /\\:") {
		return plan{}, fmt.Errorf("%q is not a usable image prefix", newImage)
	}

	return plan{oldModule: oldModule, newModule: newModule, oldImage: oldImage, newImage: newImage}, nil
}

func lastElement(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

// skip reports whether a tracked file must be left alone.
func (p plan) skip(rel string) (string, bool) {
	switch {
	case strings.HasPrefix(rel, "gen/"):
		// NEVER textually. See the package comment: the embedded descriptors are
		// length-prefixed, so a substitution here compiles and panics at init.
		return "generated: regenerated instead", true

	case strings.HasPrefix(rel, "proto/third_party/"):
		// Google's and bufbuild's protos and their licence text. Rewriting anything here
		// misattributes their work, and test/thirdparty_test.go fails the build if the
		// licence headers change.
		return "third-party: not ours to edit", true

	case rel == "LICENSE" || strings.HasSuffix(rel, "/LICENSE"):
		// No module path lives here, and a broad substitution that catches the Apache
		// appendix blanks the copyright holder on the way past -- the exact accident
		// TestRootLicenseNamesItsCopyrightHolder exists to catch.
		return "licence text", true

	case strings.HasPrefix(rel, ".git/"):
		return "git internals", true
	}
	return "", false
}

// apply rewrites every tracked file, returning the ones that changed.
func (p plan) apply(root string, files []string, dryRun bool) ([]string, error) {
	var changed []string

	for _, rel := range files {
		if _, skipped := p.skip(rel); skipped {
			continue
		}

		abs := filepath.Join(root, filepath.FromSlash(rel))
		raw, err := os.ReadFile(abs)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue // deleted between `git ls-files` and now
			}
			return nil, fmt.Errorf("read %s: %w", rel, err)
		}

		// Binary files are left alone. A module path inside one is not a Go import, and a
		// substitution of a different length corrupts whatever format it is.
		if bytes.IndexByte(raw, 0) >= 0 {
			continue
		}

		updated := p.rewrite(rel, string(raw))
		if updated == string(raw) {
			continue
		}
		changed = append(changed, rel)

		if dryRun {
			continue
		}
		// Preserve the file mode; on Windows it is 0644 either way, but a fork on Linux
		// keeps its executable bits.
		info, err := os.Stat(abs)
		if err != nil {
			return nil, fmt.Errorf("stat %s: %w", rel, err)
		}
		if err := os.WriteFile(abs, []byte(updated), info.Mode()); err != nil {
			return nil, fmt.Errorf("write %s: %w", rel, err)
		}
	}

	return changed, nil
}

// rewrite applies the substitutions for one file's content.
func (p plan) rewrite(rel, content string) string {
	content = strings.ReplaceAll(content, p.oldModule, p.newModule)

	// The image prefix is only rewritten where an image reference can legitimately appear.
	//
	// Scoped on purpose: "gomicro" is also the compose project name, a database name and a
	// Keycloak realm, and a repo-wide substitution of a bare word that short would edit prose
	// in the README and identifiers in Go source. Deployment files are where an image name
	// means an image name.
	if p.oldImage != p.newImage && isDeploymentFile(rel) {
		for _, target := range []string{"orderd", "worker", "migrate"} {
			content = strings.ReplaceAll(content,
				p.oldImage+"/"+target, p.newImage+"/"+target)
		}
	}

	return content
}

// isDeploymentFile reports whether a path may carry container image references.
func isDeploymentFile(rel string) bool {
	return strings.HasPrefix(rel, "deploy/") || rel == "Taskfile.yml"
}

// repoRoot finds the repository root from the working directory.
func repoRoot() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("this does not look like a git repository: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// requireCleanTree refuses to run over uncommitted work.
func requireCleanTree(root string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "status", "--porcelain")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("git status: %w", err)
	}
	if strings.TrimSpace(string(out)) == "" {
		return nil
	}
	return fmt.Errorf("the working tree has uncommitted changes.\n\n"+
		"This rewrites most of the repository and then deletes itself, so the only practical\n"+
		"undo is `git checkout .` -- which would take your uncommitted work with it. Commit or\n"+
		"stash first.\n\n%s", strings.TrimRight(string(out), "\n"))
}

// currentModulePath reads the module path from go.mod.
func currentModulePath(root string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return "", fmt.Errorf("read go.mod: %w", err)
	}
	for line := range strings.SplitSeq(string(raw), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.TrimSpace(rest), nil
		}
	}
	return "", errors.New("go.mod has no module line")
}

// trackedFiles lists what git knows about, which is the right set: it excludes bin/, the
// build cache and anything else .gitignore already keeps out.
func trackedFiles(root string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "ls-files", "-z")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}

	var files []string
	for _, name := range strings.Split(string(out), "\x00") {
		if name != "" {
			files = append(files, name)
		}
	}
	return files, nil
}

// regenerate runs buf, which rewrites gen/ from the protos under the new prefix.
func regenerate(root string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "go", "run", "github.com/bufbuild/buf/cmd/buf@"+bufVersion, "generate")
	cmd.Dir = root
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("regenerate protobuf code: %w", err)
	}
	return nil
}

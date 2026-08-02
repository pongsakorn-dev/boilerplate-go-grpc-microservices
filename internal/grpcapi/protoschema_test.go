package grpcapi

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

var protoPackageRE = regexp.MustCompile(`(?m)^package\s+([A-Za-z0-9_.]+)\s*;`)

// repoRoot walks up to the directory holding go.mod.
//
// testutil.RepoRoot does this already, and is not used here: this file must live in package
// grpcapi to read the unexported ownedProtoPackages, and testutil imports app, which imports
// grpcapi -- an import cycle in the test binary. Ten lines of duplication is the cheaper side
// of that trade.
func repoRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod found in any parent directory")
		}
		dir = parent
	}
}

// TestOwnedProtoPackagesMatchTheProtoDirectory closes the last hole in policy coverage.
//
// The coverage tests ask the descriptor registry "what RPCs did we declare?" -- but only for
// the packages named in ownedProtoPackages. A new proto package added to proto/ and left out
// of that list is simply never asked about, so its RPCs are never checked for an
// authorisation rule. The coverage suite would stay green while covering less.
//
// That is a nasty failure mode: the guard does not break, it quietly narrows. So the list of
// what to check is itself derived from the filesystem and compared.
func TestOwnedProtoPackagesMatchTheProtoDirectory(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	protoDir := filepath.Join(root, "proto")

	var found []string
	err := filepath.WalkDir(protoDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// third_party is vendored upstream schema -- Google's and bufbuild's packages are
		// not ours to write authorisation rules for, and none of them declare a service we
		// serve.
		if d.IsDir() && d.Name() == "third_party" {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".proto") {
			return nil
		}

		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		m := protoPackageRE.FindSubmatch(b)
		if m == nil {
			rel, _ := filepath.Rel(root, path)
			t.Errorf("%s declares no proto package", filepath.ToSlash(rel))
			return nil
		}
		if pkg := string(m[1]); !slices.Contains(found, pkg) {
			found = append(found, pkg)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk proto/: %v", err)
	}

	if len(found) == 0 {
		t.Fatal("no proto packages found under proto/ -- this guard would pass forever, and so " +
			"would every coverage assertion that depends on ownedProtoPackages")
	}

	slices.Sort(found)
	owned := slices.Clone(ownedProtoPackages)
	slices.Sort(owned)

	if !slices.Equal(found, owned) {
		t.Errorf("ownedProtoPackages is %v but proto/ declares %v.\n\n"+
			"Every package missing from the list has its RPCs silently excluded from the "+
			"policy-coverage tests: they would keep passing while checking less. Update "+
			"ownedProtoPackages in policy.go.", owned, found)
	}
}

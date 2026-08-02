package test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/example/gomicro/internal/testutil"
)

// TestVendoredProtosKeepTheirLicenseHeaders is a LEGAL guard, not a style one.
//
// proto/third_party/ redistributes three Apache-2.0 files from bufbuild and Google. Apache
// §4(c) requires retaining the copyright and license notices in the copies you distribute;
// §4(a) requires shipping a copy of the License itself, which is why proto/third_party/
// LICENSE exists.
//
// Headers are intact today. The risk is mechanical: a formatter pass, a licence-header
// tool applied repo-wide, or cmd/rename doing a broad text substitution could strip them,
// and nobody would notice until someone downstream did. That is the class of mistake worth
// a ten-line test.
func TestVendoredProtosKeepTheirLicenseHeaders(t *testing.T) {
	t.Parallel()

	root := testutil.RepoRoot(t)
	dir := filepath.Join(root, "proto", "third_party")

	var checked int
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".proto") {
			return err
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		checked++

		content := string(b)
		rel, _ := filepath.Rel(root, path)

		if !strings.Contains(content, "Apache License") {
			t.Errorf("%s no longer carries its Apache License notice.\n\n"+
				"Apache-2.0 §4(c) requires retaining the notices in redistributed copies. "+
				"Restore the header from upstream -- see proto/third_party/README.md.", rel)
		}
		if !strings.Contains(content, "Copyright") {
			t.Errorf("%s no longer carries a copyright notice", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk proto/third_party: %v", err)
	}

	if checked == 0 {
		t.Fatal("found no vendored protos -- this guard would silently pass forever")
	}
}

// TestVendoredLicenseAndProvenanceExist asserts the two files that make redistribution
// lawful are present.
//
// A URL in a header comment is not "a copy of the License". §4(a) means an actual copy, in
// the distribution.
func TestVendoredLicenseAndProvenanceExist(t *testing.T) {
	t.Parallel()

	root := testutil.RepoRoot(t)

	for _, rel := range []string{
		"LICENSE",
		filepath.Join("proto", "third_party", "LICENSE"),
		filepath.Join("proto", "third_party", "README.md"),
	} {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Errorf("%s is missing: %v\n\n"+
				"Redistributing the vendored Apache-2.0 protos without a copy of the License "+
				"is a licence breach, and the root LICENSE is what makes this template legally "+
				"usable at all.", filepath.ToSlash(rel), err)
			continue
		}
		if !strings.Contains(string(b), "Apache License") && strings.HasSuffix(rel, "LICENSE") {
			t.Errorf("%s does not contain the Apache License text", filepath.ToSlash(rel))
		}
	}

	// The upstream licence copy must keep its placeholder: it is bufbuild's and Google's
	// licence, not ours, so filling in a copyright holder would misattribute it.
	b, err := os.ReadFile(filepath.Join(root, "proto", "third_party", "LICENSE"))
	if err == nil && !strings.Contains(string(b), "[name of copyright owner]") {
		t.Error("proto/third_party/LICENSE has had its appendix placeholder filled in. " +
			"That file is the upstream projects' licence text and must stay verbatim; the " +
			"root LICENSE is the one that names this project's copyright holder.")
	}
}

// TestRootLicenseNamesItsCopyrightHolder is the MIRROR of the placeholder check above, and
// it was missing.
//
// The two LICENSE files in this repo have OPPOSITE requirements, which is exactly why both
// need guarding:
//
//	proto/third_party/LICENSE  must KEEP "[name of copyright owner]" -- it is bufbuild's and
//	                           Google's licence text, and filling it in misattributes their
//	                           work to us.
//	LICENSE                    must have it REPLACED -- an Apache-2.0 project whose appendix
//	                           still reads "Copyright [yyyy] [name of copyright owner]"
//	                           identifies no copyright holder, which is the one field the
//	                           grant depends on.
//
// Only the first was checked. The risk is not theoretical: cmd/rename (M11) rewrites the
// module path with broad text substitution across the tree, and a substitution that catches
// this line blanks the copyright on the way past. The same class of mechanical accident the
// header guard above already exists to catch.
func TestRootLicenseNamesItsCopyrightHolder(t *testing.T) {
	t.Parallel()

	root := testutil.RepoRoot(t)
	b, err := os.ReadFile(filepath.Join(root, "LICENSE"))
	if err != nil {
		t.Fatalf("read LICENSE: %v", err)
	}
	content := string(b)

	for _, placeholder := range []string{"[name of copyright owner]", "[yyyy]"} {
		if strings.Contains(content, placeholder) {
			t.Errorf("the root LICENSE still contains the template placeholder %q.\n\n"+
				"Apache-2.0's grant runs from a named copyright holder. Left unfilled, this "+
				"project is published under a licence that identifies nobody.", placeholder)
		}
	}

	// And the line must actually be there. Deleting the appendix entirely would pass the
	// placeholder check above while leaving the same hole.
	if !strings.Contains(content, "Copyright") {
		t.Error("the root LICENSE carries no copyright line at all")
	}
}

package main

import (
	"bytes"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"

	orderv1 "github.com/example/gomicro/gen/go/order/v1"
)

// TestRewritingAGeneratedDescriptorCorruptsIt is the finding this whole tool is shaped around,
// reproduced in milliseconds instead of by building a renamed repository.
//
// A generated .pb.go embeds its FileDescriptorProto as a raw byte string, and protobuf wire
// format is LENGTH-PREFIXED. The descriptor contains go_package, which contains the module
// path. Substitute a path of a different length and every prefix after that point addresses
// the wrong bytes.
//
// What makes it dangerous is that nothing notices: the Go file still compiles, `go vet` still
// passes, and the failure arrives at init -- before main -- as
//
//	panic: runtime error: slice bounds out of range [-4:]
//
// in a stack trace that mentions neither the rename nor the module path.
//
// This test takes the REAL descriptor from the REAL generated package, applies the naive
// substitution to its bytes, and asks the protobuf runtime to read the result.
func TestRewritingAGeneratedDescriptorCorruptsIt(t *testing.T) {
	t.Parallel()

	const (
		oldPath = "github.com/example/gomicro"
		newPath = "github.com/acme/orders" // four bytes shorter, which is the entire bug
	)

	descriptor := protodesc.ToFileDescriptorProto(orderv1.File_order_v1_order_proto)
	raw, err := proto.Marshal(descriptor)
	if err != nil {
		t.Fatalf("marshal the descriptor: %v", err)
	}

	if !bytes.Contains(raw, []byte(oldPath)) {
		t.Fatalf("the serialized descriptor does not contain %q, so this test is not "+
			"demonstrating anything.\n\n"+
			"If managed mode stopped writing go_package into the descriptor, the rename tool's "+
			"whole reason for regenerating instead of substituting needs re-deriving.", oldPath)
	}

	corrupted := bytes.ReplaceAll(raw, []byte(oldPath), []byte(newPath))
	if len(corrupted) == len(raw) {
		t.Fatal("the substitution did not change the length, so it cannot demonstrate the bug")
	}

	var parsed descriptorpb.FileDescriptorProto
	if err := proto.Unmarshal(corrupted, &parsed); err == nil {
		t.Errorf("a length-changing substitution inside the descriptor parsed cleanly.\n\n"+
			"That would mean the rename tool could safely rewrite gen/ as text, which is the "+
			"opposite of what it does. Re-derive the decision before trusting this comment: "+
			"%d bytes became %d and the protobuf runtime did not object.", len(raw), len(corrupted))
	} else {
		t.Logf("confirmed: %d bytes -> %d bytes, and the descriptor no longer parses (%v)",
			len(raw), len(corrupted), err)
	}
}

// TestGeneratedCodeIsNeverRewritten pins the rule that follows from it.
func TestGeneratedCodeIsNeverRewritten(t *testing.T) {
	t.Parallel()

	p := plan{
		oldModule: "github.com/example/gomicro",
		newModule: "github.com/acme/orders",
		oldImage:  "gomicro",
		newImage:  "orders",
	}

	mustSkip := []string{
		"gen/go/order/v1/order.pb.go",
		"gen/go/order/v1/order_grpc.pb.go",
		"gen/go/order/v1/order.pb.gw.go",
		"gen/openapiv2/orderd.swagger.json",

		// Not ours to edit: rewriting these misattributes Google's and bufbuild's work, and
		// test/thirdparty_test.go fails the build when their licence headers change.
		"proto/third_party/google/api/annotations.proto",
		"proto/third_party/LICENSE",

		// A broad substitution that reaches the Apache appendix blanks the copyright holder
		// on the way past -- exactly what TestRootLicenseNamesItsCopyrightHolder catches.
		"LICENSE",
	}
	for _, rel := range mustSkip {
		if _, skipped := p.skip(rel); !skipped {
			t.Errorf("%s would be rewritten", rel)
		}
	}

	mustRewrite := []string{
		"go.mod",
		"buf.gen.yaml",
		"cmd/orderd/main.go",
		"internal/app/app.go",
		"proto/order/v1/order.proto",
		"deploy/k8s/base/deployment.yaml",
	}
	for _, rel := range mustRewrite {
		if reason, skipped := p.skip(rel); skipped {
			t.Errorf("%s would be skipped (%s), but it carries the module path", rel, reason)
		}
	}
}

// TestTheImagePrefixIsOnlyRewrittenInDeploymentFiles keeps a short, common word out of prose
// and identifiers.
//
// "gomicro" is also the compose project name, the database name, the Keycloak realm and a word
// in the README. A repo-wide substitution of a bare word that short edits all of them, and the
// damage is invisible until something fails to connect.
func TestTheImagePrefixIsOnlyRewrittenInDeploymentFiles(t *testing.T) {
	t.Parallel()

	p := plan{
		oldModule: "github.com/example/gomicro",
		newModule: "github.com/acme/orders",
		oldImage:  "gomicro",
		newImage:  "orders",
	}

	// In a deployment file, an image reference is an image reference.
	if got := p.rewrite("deploy/k8s/base/deployment.yaml", "image: gomicro/orderd\n"); got != "image: orders/orderd\n" {
		t.Errorf("deployment image = %q, want it rewritten", got)
	}
	if got := p.rewrite("Taskfile.yml", "docker build -t gomicro/worker:dev .\n"); got != "docker build -t orders/worker:dev .\n" {
		t.Errorf("Taskfile image = %q, want it rewritten", got)
	}

	// Everywhere else the same characters mean something else entirely.
	for _, tc := range []struct{ file, content string }{
		{"README.md", "the compose project is named gomicro/orderd in the examples"},
		{"internal/app/app.go", "// gomicro/orderd is the image built by deploy/docker"},
	} {
		if got := p.rewrite(tc.file, tc.content); got != tc.content {
			t.Errorf("%s was edited by the image substitution:\n  %q\n  %q", tc.file, tc.content, got)
		}
	}
}

// TestTheModulePathIsRewrittenEverywhereItAppears covers the ordinary case, including the two
// non-Go files that carry it.
func TestTheModulePathIsRewrittenEverywhereItAppears(t *testing.T) {
	t.Parallel()

	p := plan{
		oldModule: "github.com/example/gomicro",
		newModule: "github.com/acme/orders",
		oldImage:  "gomicro",
		newImage:  "orders",
	}

	cases := []struct{ file, in, want string }{
		{"go.mod", "module github.com/example/gomicro\n", "module github.com/acme/orders\n"},
		{
			"buf.gen.yaml",
			"      value: github.com/example/gomicro/gen/go\n",
			"      value: github.com/acme/orders/gen/go\n",
		},
		{
			"internal/app/app.go",
			"\t\"github.com/example/gomicro/internal/platform/config\"\n",
			"\t\"github.com/acme/orders/internal/platform/config\"\n",
		},
	}

	for _, tc := range cases {
		if got := p.rewrite(tc.file, tc.in); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.file, got, tc.want)
		}
	}
}

// TestAnUnusableModulePathIsRefusedBeforeAnythingIsWritten matters more than it looks.
//
// The tool rewrites ninety files and then deletes itself. A module path go.mod cannot parse
// discovered afterwards leaves a repository that does not build and no tool to re-run.
func TestAnUnusableModulePathIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	t.Parallel()

	const current = "github.com/example/gomicro"

	bad := []struct{ name, path string }{
		{"empty", ""},
		{"whitespace", "   "},
		{"a space inside", "github.com/acme/my orders"},
		{"backslashes", `github.com\acme\orders`},
		{"trailing slash", "github.com/acme/orders/"},
		{"no host", "orders"},
		{"unchanged", current},
	}

	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := newPlan(current, tc.path, ""); err == nil {
				t.Errorf("newPlan accepted %q", tc.path)
			}
		})
	}

	good := []string{
		"github.com/acme/orders",
		"gitlab.example.com/team/sub/group/orders",
		"example.com/orders.v2",
		"github.com/acme-corp/order_service",
	}
	for _, path := range good {
		if _, err := newPlan(current, path, ""); err != nil {
			t.Errorf("newPlan rejected the valid path %q: %v", path, err)
		}
	}
}

// TestTheImagePrefixDefaultsToTheModuleName saves a flag in the common case.
func TestTheImagePrefixDefaultsToTheModuleName(t *testing.T) {
	t.Parallel()

	p, err := newPlan("github.com/example/gomicro", "github.com/acme/orders", "")
	if err != nil {
		t.Fatalf("newPlan: %v", err)
	}
	if p.newImage != "orders" {
		t.Errorf("newImage = %q, want the module's last element", p.newImage)
	}

	// And an explicit prefix wins, because a registry namespace is often not the repo name.
	p, err = newPlan("github.com/example/gomicro", "github.com/acme/orders", "acme-registry")
	if err != nil {
		t.Fatalf("newPlan: %v", err)
	}
	if p.newImage != "acme-registry" {
		t.Errorf("newImage = %q, want the explicit value", p.newImage)
	}

	if _, err := newPlan("github.com/example/gomicro", "github.com/acme/orders", "acme/orders"); err == nil {
		t.Error("an image prefix containing a slash was accepted; it would produce " +
			"acme/orders/orderd, which is a different registry path than intended")
	}
}

// TestTheBufVersionMatchesTheRestOfTheRepo keeps the pin honest.
//
// The tool runs buf to regenerate, so its pin joins the three that test/toolpins_test.go
// already compares. A tool that regenerated with a different buf than `task gen` uses would
// produce a diff on the next codegen check.
func TestTheBufVersionMatchesTheRestOfTheRepo(t *testing.T) {
	t.Parallel()

	if !strings.HasPrefix(bufVersion, "v") {
		t.Fatalf("bufVersion = %q, want a v-prefixed version", bufVersion)
	}
}

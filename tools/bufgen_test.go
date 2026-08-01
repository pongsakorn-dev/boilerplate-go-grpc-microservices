package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/example/gomicro/internal/testutil"
)

// TestBufGenUsesNoProtocPlugins keeps the "Go toolchain only" promise enforceable.
//
// buf.gen.yaml supports `protoc_builtin` plugins and a `protoc_path`, both of which shell
// out to the real protoc. protoc is a C++ binary: it cannot be `go install`ed, it is not in
// the module cache, and it has to arrive via a system package manager. Adding one would
// silently break the single prerequisite this template advertises -- Go, and nothing else --
// for every person who clones it.
//
// The failure would only appear on a machine without protoc, which is never the machine of
// the person who added it.
func TestBufGenUsesNoProtocPlugins(t *testing.T) {
	t.Parallel()

	b, err := os.ReadFile(filepath.Join(testutil.RepoRoot(t), "buf.gen.yaml"))
	if err != nil {
		t.Fatalf("read buf.gen.yaml: %v", err)
	}
	content := string(b)

	banned := []struct{ needle, why string }{
		{"protoc_builtin", "protoc_builtin plugins require the protoc binary, which cannot be go-installed"},
		{"protoc_path", "protoc_path points at a C++ binary that a fresh clone will not have"},
	}

	for _, ban := range banned {
		if strings.Contains(content, ban.needle) {
			t.Errorf("buf.gen.yaml uses %q: %s\n\n"+
				"Use a `local:` plugin resolved through the go.mod tool directive instead, e.g.\n"+
				"    - local: [go, tool, protoc-gen-go]",
				ban.needle, ban.why)
		}
	}
}

// TestBufYamlHasNoRegistryDeps keeps codegen genuinely offline.
//
// A `deps:` entry in buf.yaml resolves from the Buf Schema Registry into buf's own cache
// under %LOCALAPPDATA% (or ~/.cache). `go mod download` never warms that cache, so a
// declared dependency makes `task gen` require network access and BSR availability on every
// cold machine -- and it transmits your schema to a third party on every generate.
//
// Third-party protos are vendored under proto/third_party/ instead.
func TestBufYamlHasNoRegistryDeps(t *testing.T) {
	t.Parallel()

	root := testutil.RepoRoot(t)

	b, err := os.ReadFile(filepath.Join(root, "buf.yaml"))
	if err != nil {
		t.Fatalf("read buf.yaml: %v", err)
	}

	for _, line := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "deps:") {
			t.Error("buf.yaml declares BSR `deps:`.\n\n" +
				"That makes `task gen` need the network on a cold machine and sends the schema\n" +
				"to the registry on every run. Vendor the .proto into proto/third_party/ instead.")
		}
	}

	if _, err := os.Stat(filepath.Join(root, "buf.lock")); err == nil {
		t.Error("buf.lock exists, which means BSR dependencies are in use; " +
			"vendor them into proto/third_party/ instead")
	}
}

// TestVendoredProtosArePresent fails loudly if a vendored import disappears.
//
// Without these files buf generate fails with an import-resolution error that reads like a
// bug in the protos rather than a missing vendored file.
func TestVendoredProtosArePresent(t *testing.T) {
	t.Parallel()

	root := testutil.RepoRoot(t)
	required := []string{
		"proto/third_party/buf/validate/validate.proto",
		"proto/third_party/google/api/annotations.proto",
		"proto/third_party/google/api/http.proto",
	}

	for _, rel := range required {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Errorf("vendored proto missing: %s\n\n"+
				"Restore it with:\n"+
				"  go run github.com/bufbuild/buf/cmd/buf@v1.72.0 export buf.build/bufbuild/protovalidate -o <tmp>",
				rel)
		}
	}
}

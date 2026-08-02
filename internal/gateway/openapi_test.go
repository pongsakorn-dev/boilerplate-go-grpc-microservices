package gateway_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/example/gomicro/internal/platform/auth/testjwks"
	"github.com/example/gomicro/internal/testutil"
)

// The OpenAPI document is a PROMISE about the running service, and nothing compiles it.
//
// It is generated from the same .proto, so the paths and types are right by construction.
// What is NOT right by construction is field naming: protoc-gen-openapiv2 defaults to
// lowerCamelCase, while internal/gateway sets protojson's UseProtoNames and emits snake_case.
// The two are configured in different files, by different tools, and a mismatch produces a
// schema that is wrong about the name of every multi-word field -- so every client generated
// from it breaks on first contact, and the document still looks perfectly plausible.
//
// So this compares the document against an actual response rather than against itself.

func loadSwagger(t *testing.T) map[string]any {
	t.Helper()

	path := filepath.Join("..", "..", "gen", "openapiv2", "orderd.swagger.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the OpenAPI document: %v\n\nRun `go tool task gen` to generate it.", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("the OpenAPI document is not valid JSON: %v", err)
	}
	return doc
}

// TestOpenAPIFieldNamesMatchWhatTheServerEmits binds the document to observed behaviour.
func TestOpenAPIFieldNamesMatchWhatTheServerEmits(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join("..", "..", "gen", "openapiv2", "orderd.swagger.json"))
	if err != nil {
		t.Fatalf("read the OpenAPI document: %v", err)
	}
	schema := string(raw)

	iss := testjwks.New(t)
	srv, _ := testutil.NewTestGateway(t, oidcMode(iss))
	auth := bearer(iss.Sign(iss.DefaultClaims()))

	created := `{"customer_id":"cust-openapi","items":[{"sku":"SKU-1","quantity":1,` +
		`"unit_price":{"currency_code":"USD","units":"7"}}]}`

	status, body, respRaw := doJSON(t, srv.URL, http.MethodPost, "/v1/orders", created, auth)
	if status != http.StatusOK {
		t.Fatalf("POST /v1/orders returned %d: %s", status, respRaw)
	}

	order, _ := body["order"].(map[string]any)
	if len(order) == 0 {
		t.Fatalf("no order in the response, so there are no field names to check: %s", respRaw)
	}

	var checked int
	for field := range order {
		// Only multi-word fields can disagree; single words are identical in both
		// conventions and would make this pass vacuously.
		if !strings.Contains(field, "_") {
			continue
		}
		checked++

		if !strings.Contains(schema, `"`+field+`"`) {
			t.Errorf("the server emits %q but the OpenAPI document never mentions it.\n\n"+
				"Almost certainly json_names_for_fields drifted back to its default in "+
				"buf.gen.yaml, so the schema documents lowerCamelCase while the server emits "+
				"proto names. Every client generated from this document would be wrong.", field)
		}
	}

	if checked == 0 {
		t.Fatal("no multi-word fields in the response, so this comparison proves nothing")
	}
}

// TestOpenAPIDocumentsEveryRESTPath catches an RPC gaining an http binding that never reaches
// the published schema -- an endpoint that exists and is undiscoverable.
func TestOpenAPIDocumentsEveryRESTPath(t *testing.T) {
	t.Parallel()

	doc := loadSwagger(t)
	paths, _ := doc["paths"].(map[string]any)
	if len(paths) == 0 {
		t.Fatal("the OpenAPI document declares no paths, so every assertion here is vacuous")
	}

	// Every binding in order.proto, listed explicitly. Deriving them from the document would
	// only prove the document agrees with itself.
	want := []string{
		"/v1/orders",
		"/v1/orders/{order_id}",
		"/v1/orders/{order_id}:cancel",
		"/v1/orders:watch",
	}
	for _, p := range want {
		if _, ok := paths[p]; !ok {
			t.Errorf("the OpenAPI document has no path %q.\n\ndocumented: %v", p, keysOf(paths))
		}
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

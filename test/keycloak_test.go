package test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/example/gomicro/internal/grpcapi"
	"github.com/example/gomicro/internal/testutil"
)

// The Keycloak realm export is CONFIGURATION, and configuration rots exactly like code --
// except nothing compiles it, so nothing tells you.
//
// What this file can prove: the realm still contains the mappers and scopes that make it
// work with this service. What it CANNOT prove: that Keycloak accepts the file at all. It
// has never been imported by a running Keycloak -- Docker was unavailable when it was
// written -- and deploy/keycloak/README.md says so in a box that should be deleted the day
// somebody boots it.
//
// A structural check is still worth having. The realistic failure is not "Keycloak rejects
// the JSON", which anyone trying it discovers in thirty seconds. It is somebody editing the
// realm months from now, dropping the audience mapper, and shipping a file that imports
// cleanly and mints tokens this service rejects with an error pointing nowhere useful.

type kcRealm struct {
	Realm        string `json:"realm"`
	ClientScopes []struct {
		Name string `json:"name"`
	} `json:"clientScopes"`
	Clients []struct {
		ClientID             string `json:"clientId"`
		BearerOnly           bool   `json:"bearerOnly"`
		ServiceAccountsEnbld bool   `json:"serviceAccountsEnabled"`
		StandardFlowEnabled  bool   `json:"standardFlowEnabled"`
		ProtocolMappers      []struct {
			Name           string            `json:"name"`
			ProtocolMapper string            `json:"protocolMapper"`
			Config         map[string]string `json:"config"`
		} `json:"protocolMappers"`
	} `json:"clients"`
	Users []struct {
		Username   string              `json:"username"`
		Attributes map[string][]string `json:"attributes"`
	} `json:"users"`
}

func loadRealm(t *testing.T) kcRealm {
	t.Helper()

	path := filepath.Join(testutil.RepoRoot(t), "deploy", "keycloak", "realm-export.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read realm export: %v", err)
	}

	var realm kcRealm
	if err := json.Unmarshal(b, &realm); err != nil {
		t.Fatalf("the realm export is not valid JSON, so Keycloak cannot import it: %v", err)
	}
	return realm
}

// TestRealmSetsTheAudienceOnEveryTokenIssuingClient guards the single most common OIDC
// integration failure.
//
// A Keycloak access token's default `aud` is "account", NOT your API. Without an explicit
// audience mapper every request fails audience validation, and the error sends people
// looking at issuer URLs and clocks rather than at a missing mapper.
func TestRealmSetsTheAudienceOnEveryTokenIssuingClient(t *testing.T) {
	t.Parallel()

	realm := loadRealm(t)

	var checked int
	for _, client := range realm.Clients {
		// bearerOnly clients never request tokens; they exist to BE an audience.
		if client.BearerOnly {
			continue
		}
		if !client.ServiceAccountsEnbld && !client.StandardFlowEnabled {
			continue
		}
		checked++

		var found bool
		for _, m := range client.ProtocolMappers {
			if m.ProtocolMapper == "oidc-audience-mapper" &&
				m.Config["included.client.audience"] == "orderd" &&
				m.Config["access.token.claim"] == "true" {
				found = true
			}
		}
		if !found {
			t.Errorf("client %q issues tokens but has no audience mapper for \"orderd\".\n\n"+
				"Keycloak defaults the audience to \"account\", so every token this client mints "+
				"is rejected by OIDC_AUDIENCE=orderd -- with an error that points at nothing.",
				client.ClientID)
		}
	}

	if checked == 0 {
		t.Fatal("no token-issuing clients found in the realm; this guard would pass forever")
	}
}

// TestRealmPutsATenantOnEveryToken enforces the rule the whole auth package exists for: the
// tenant comes from the verified token.
//
// A token with no tenant claim is refused by the verifier, deliberately and with a message
// naming the claim. This catches the same mistake one layer earlier, in the file that causes
// it.
func TestRealmPutsATenantOnEveryToken(t *testing.T) {
	t.Parallel()

	realm := loadRealm(t)

	for _, client := range realm.Clients {
		if client.BearerOnly || (!client.ServiceAccountsEnbld && !client.StandardFlowEnabled) {
			continue
		}

		var found bool
		for _, m := range client.ProtocolMappers {
			if m.Config["claim.name"] == "tenant_id" && m.Config["access.token.claim"] == "true" {
				found = true
			}
		}
		if !found {
			t.Errorf("client %q issues tokens with no tenant_id claim.\n\n"+
				"The verifier refuses such tokens, so this client cannot call the service at all. "+
				"Machine clients want oidc-hardcoded-claim-mapper; user-facing clients want "+
				"oidc-usermodel-attribute-mapper reading the user's tenant_id attribute.", client.ClientID)
		}
	}
}

// TestRealmScopesMatchThePolicy is the join between the realm and the code.
//
// The policy denies a caller lacking a scope. If Keycloak cannot issue that scope, the
// endpoint is unreachable by anyone -- a failure that looks like a bug in the service and
// lives entirely in the realm file.
func TestRealmScopesMatchThePolicy(t *testing.T) {
	t.Parallel()

	realm := loadRealm(t)

	available := make(map[string]bool, len(realm.ClientScopes))
	for _, s := range realm.ClientScopes {
		available[s.Name] = true
	}

	// Read the required scopes from the policy itself rather than restating them, so the
	// two cannot drift.
	required := make(map[string]bool)
	for _, rule := range grpcapi.DefaultPolicy() {
		for _, scope := range rule.Scopes {
			required[scope] = true
		}
	}
	if len(required) == 0 {
		t.Fatal("the policy requires no scopes at all; this guard would pass vacuously")
	}

	for scope := range required {
		if !available[scope] {
			t.Errorf("the policy requires scope %q, which the Keycloak realm cannot issue.\n\n"+
				"Every endpoint gated on it is unreachable by any caller. Add it to clientScopes "+
				"in deploy/keycloak/realm-export.json and to the relevant client's "+
				"defaultClientScopes.", scope)
		}
	}
}

// TestRealmUsersSpanTwoTenants keeps the realm useful for exercising isolation by hand.
//
// One tenant proves nothing: every cross-tenant bug is invisible until a second tenant
// exists. The automated equivalent is
// grpcapi/auth_test.go::TestTenantIsolationIsDrivenByTheToken.
func TestRealmUsersSpanTwoTenants(t *testing.T) {
	t.Parallel()

	realm := loadRealm(t)

	tenants := make(map[string]bool)
	for _, u := range realm.Users {
		for _, v := range u.Attributes["tenant_id"] {
			tenants[v] = true
		}
	}

	if len(tenants) < 2 {
		t.Errorf("the realm's users span %d tenant(s), want at least 2. A single-tenant realm "+
			"cannot demonstrate the isolation this service's whole identity model is built on.",
			len(tenants))
	}
}

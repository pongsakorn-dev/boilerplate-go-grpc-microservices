package grpcapi

import (
	"google.golang.org/grpc"

	"github.com/example/gomicro/internal/platform/auth"
)

// Scopes this service recognises.
//
// Constants rather than string literals so a typo in a rule is a compile error instead of a
// rule that silently matches nothing. A misspelled scope in a Rule denies everyone, which is
// at least safe; a misspelled scope in a token check would be worse.
const (
	ScopeOrdersRead  = "orders:read"
	ScopeOrdersWrite = "orders:write"
)

// ownedProtoPackages are the proto packages this repo defines.
//
// The coverage test uses this to ask the descriptor registry "what RPCs did we declare?",
// which is a different question from "what is this server serving?" -- see
// policy_coverage_test.go. Adding a proto package without adding it here means its RPCs are
// never checked for policy coverage, so protoschema_test.go asserts this list matches the
// packages under proto/.
var ownedProtoPackages = []string{"order.v1"}

// DefaultPolicy is the authorisation decision for every RPC this server exposes.
//
// THIS MAP IS THE SECURITY BOUNDARY. Adding an RPC without adding a line here does not
// produce a permissive default -- it produces a server that refuses to start, because
// NewServer calls ValidateCoverage. That is the intended experience: the build stops and
// tells you to make a decision, at the moment you are already editing this area.
//
// Returned fresh rather than exposed as a package-level var: a map var is mutable by any
// package that can see it, and "the authorisation policy is a global anyone can write to"
// is not a sentence worth defending.
func DefaultPolicy() auth.Policy {
	return auth.Policy{
		// Reads and writes are separated so a reporting or analytics client can be issued a
		// credential that genuinely cannot mutate anything. A single "orders" scope would
		// make that impossible to express, and the cost of splitting later is reissuing
		// every token in circulation.
		"/order.v1.OrderService/GetOrder":    {Scopes: []string{ScopeOrdersRead}},
		"/order.v1.OrderService/ListOrders":  {Scopes: []string{ScopeOrdersRead}},
		"/order.v1.OrderService/WatchOrders": {Scopes: []string{ScopeOrdersRead}},
		"/order.v1.OrderService/CreateOrder": {Scopes: []string{ScopeOrdersWrite}},

		// CancelOrder is a write, not a read. Stated because "cancel" reads like a
		// lifecycle operation rather than a mutation, and grouping it with the reads is an
		// easy and expensive mistake.
		"/order.v1.OrderService/CancelOrder": {Scopes: []string{ScopeOrdersWrite}},

		// Health is PUBLIC, and it has to be: a kubelet holds no credential and cannot be
		// given one. Kubernetes' native grpc: probe dials the port and calls Check with no
		// metadata at all, so any rule requiring authentication here makes every pod fail
		// its readiness probe and never join the Service -- a self-inflicted outage that
		// looks like a crashloop.
		//
		// The exposure is real but small: an unauthenticated caller learns the service is
		// alive. That is also what a TCP connect tells them.
		"/grpc.health.v1.Health/Check": {Public: true},
		"/grpc.health.v1.Health/Watch": {Public: true},

		// List is NOT public, and it is in this file because ValidateCoverage refused to
		// let the server start without it.
		//
		// That is worth recording. This policy was written by reading order.proto and
		// naming the health methods from memory -- Check and Watch. grpc-go also registers
		// List, which appears in no .proto in this repo, and the first run of the server
		// failed with "served but NOT in the policy: /grpc.health.v1.Health/List". A policy
		// derived from the schema alone would have missed it; a default-open server would
		// have exposed it silently.
		//
		// It is authenticated because, unlike Check, nothing automated needs it: it
		// enumerates every registered service and its status, and no probe calls it.
		"/grpc.health.v1.Health/List": {},

		// Reflection requires authentication but no scopes.
		//
		// It hands the caller your entire schema -- every service, message and field name.
		// That is exactly what makes `grpcurl -plaintext localhost:50051 list` work with no
		// .proto file, and exactly why it should not be anonymous. Authenticated-but-
		// unscoped is the balance: any legitimate caller can explore, no stranger can
		// enumerate. A service handling sensitive schemas should stop registering
		// reflection at all rather than tightening this rule -- see chain.go.
		"/grpc.reflection.v1.ServerReflection/ServerReflectionInfo":      {},
		"/grpc.reflection.v1alpha.ServerReflection/ServerReflectionInfo": {},
	}
}

// servedMethods lists every method registered on a server, as full gRPC method names.
//
// This is the ground truth for coverage: it reflects what the server will actually dispatch,
// including the health and reflection services that grpc-go registers on our behalf and that
// no .proto in this repo mentions.
func servedMethods(srv *grpc.Server) []string {
	var out []string
	for name, info := range srv.GetServiceInfo() {
		for _, m := range info.Methods {
			out = append(out, "/"+name+"/"+m.Name)
		}
	}
	return out
}

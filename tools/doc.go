// Package tools holds guards over the proto toolchain itself.
//
// Most of the tests here carry the `codegen` build tag because they exec buf, which needs
// the network on a cold module cache. This file exists so the package has a non-test Go
// file: a directory containing only _test.go files makes `go build ./...` report
// "no non-test Go files", which would fail the default verify target for no good reason.
package tools

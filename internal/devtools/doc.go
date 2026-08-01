// Package devtools holds guards over the developer tooling itself.
//
// It has no runtime code. This file exists so the package has a non-test Go file, because
// a directory containing only _test.go files makes `go build ./...` report
// "no non-test Go files".
package devtools

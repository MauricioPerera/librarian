// Package pgtestdb is TEST INFRASTRUCTURE ONLY: it provisions the isolated
// PostgreSQL target that the dual-engine batteries run against. Nothing in
// librarian's production path imports it.
//
// Its whole implementation sits behind the `dualengine` build tag, next to the
// batteries that are the only consumers, so the default build never links a
// package that imports `testing`. This file carries no tag purely so the
// directory always has one buildable Go file and `go build ./...` /
// `go vet ./...` keep working without the tag.
//
// CONTRACT-29. See pgtestdb.go for the isolation mechanism and why it is a
// DATABASE per run and not a schema per run.
package pgtestdb

// Package catalog resolves reviewed journey selectors against a snapshot of
// the tools an integration server advertises.
//
// It replaces internal/mcp as the owner of the tool surface a journey may
// expose to the model. Resolution is pure: it performs no I/O, holds no
// package state, and returns the same Resolution for the same inputs
// regardless of the order the server listed its tools.
//
// A snapshot is evidence of what the server advertised at one time. It is
// never authorization: the class and approval requirement of a tool come
// only from the reviewed journey definition, and MCP annotations (such as
// readOnlyHint) can narrow a selection but never grant a class.
package catalog

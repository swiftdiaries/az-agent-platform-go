// Package mcptest provides a reusable go-sdk streamable-HTTP MCP test server
// that runs in httptest.
//
// A [Server] serves per-tenant, per-principal, per-version tool catalogs
// ([Catalog]) over the real MCP streamable-HTTP transport, so tests exercise
// the genuine client and server SDK stacks. It exposes:
//
//   - observation: every inbound HTTP request is recorded ([Server.Requests]),
//     and JSON-RPC methods, tool call attempts and applied effects are counted
//     ([Server.Count], [Server.Calls], [Server.Applied]);
//   - fault injection keyed by remote tool name: [Server.Block],
//     [Server.FailTransient], [Server.DropResponseAfterApply],
//     [Server.IsError] and [Server.Unauthorized].
//
// Remote tool names are served verbatim, including names the SDK considers
// invalid ("billing/refund", "lookup destination", "ns:op"); the SDK only logs
// those at error level, and the test server discards that log.
package mcptest

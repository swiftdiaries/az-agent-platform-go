# Catalog fixtures

Each `<version>/tools.json` is a recorded MCP `tools/list` response (a JSON array of
tool descriptors, as the go-sdk `mcp.Tool` marshals them) for one supported product
version of the integration endpoint. The directory name is the product version.

`TestJourneysResolveAgainstSupportedVersions` (`internal/catalog/versions_test.go`)
resolves every journey in `configs/journeys.yaml` against every version here whose
range admits it. CI fails on unexpected unavailability, ambiguity, alias collision,
`max_tools` overflow, or an alias that differs between versions.

The support window is the current product version plus the two before it (proposed).

## Adding a version

1. Record `tools/list` from the new product version and save the array as
   `testdata/catalogs/<version>/tools.json` (keep `_meta.operation_key`, `_meta.tags`
   and `annotations` as served; strip nothing the resolver reads).
2. Run `go test ./internal/catalog -run TestJourneysResolveAgainstSupportedVersions`.
3. Drop the oldest directory when it leaves the support window.

The fixtures here are synthetic stand-ins until real recordings are available.

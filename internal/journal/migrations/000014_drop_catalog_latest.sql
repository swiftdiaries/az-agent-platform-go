-- The per-principal "latest" pointer had no production reader; conversations pin their own digest.
DROP TABLE agent_catalog_latest;

-- 003_package_metadata.sql
-- Store the DESCRIPTION fields PACKAGES needs. Without Depends /
-- Imports / LinkingTo in the index, install.packages() cannot pull in
-- a package's dependencies from the same repository.
--
-- packages.index_fields: JSON object of the standard repository
--   fields (see internal/rpkg.IndexFields) read from the source
--   tarball. NULL means "not extracted yet" (rows from before this
--   migration; the startup backfill fills them); '{}' means the
--   tarball had no readable DESCRIPTION.
-- binaries.built: the binary tarball's DESCRIPTION Built field. NULL
--   means not extracted yet; '' means none found.

ALTER TABLE packages ADD COLUMN index_fields TEXT;

ALTER TABLE binaries ADD COLUMN built TEXT;

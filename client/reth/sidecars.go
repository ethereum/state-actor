package reth

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	iReth "github.com/nerolation/state-actor/internal/reth"
)

// WriteDatabaseVersion writes <dbDir>/database.version with the canonical
// reth schema-version string (currently "2"). dbDir is the MDBX directory
// (typically <datadir>/db/), NOT the parent datadir.
//
// Reth boot opens this file via crates/storage/db/src/version.rs;
// mismatch fails with VersionMismatch.
func WriteDatabaseVersion(dbDir string) error {
	path := filepath.Join(dbDir, "database.version")
	content := strconv.Itoa(iReth.DBVersion)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

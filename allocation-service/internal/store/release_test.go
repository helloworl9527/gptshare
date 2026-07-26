package store

import "testing"

func TestPrintLatestSchemaVersionForRelease(t *testing.T) {
	version, err := LatestSchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("schema_version=%d", version)
}

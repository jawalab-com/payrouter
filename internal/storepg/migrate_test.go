package storepg

import "testing"

func TestEmbeddedMigrationVersionsAreUnique(t *testing.T) {
	files, err := migrationFiles()
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[int]string, len(files))
	for _, file := range files {
		if previous := seen[file.version]; previous != "" {
			t.Fatalf("migration version %d reused by %s and %s", file.version, previous, file.name)
		}
		seen[file.version] = file.name
	}
}

package store

import (
	"testing"
	"testing/fstest"
)

func TestLoadMigrations(t *testing.T) {
	t.Run("embedded set is valid", func(t *testing.T) {
		migs, err := loadMigrations(migrationsFS)
		if err != nil || len(migs) == 0 {
			t.Fatalf("loadMigrations = %d, %v", len(migs), err)
		}
	})

	fsOf := func(names ...string) fstest.MapFS {
		m := fstest.MapFS{}
		for _, n := range names {
			m["migrations/"+n] = &fstest.MapFile{Data: []byte("SELECT 1;")}
		}
		return m
	}
	cases := []struct {
		name    string
		files   []string
		wantErr bool
	}{
		{"contiguous", []string{"0001_a.sql", "0002_b.sql"}, false},
		{"out of order on disk is sorted", []string{"0002_b.sql", "0001_a.sql"}, false},
		{"gap", []string{"0001_a.sql", "0003_c.sql"}, true},
		{"starts at 2", []string{"0002_b.sql"}, true},
		{"duplicate version", []string{"0001_a.sql", "0001_b.sql"}, true},
		{"no numeric prefix", []string{"init.sql"}, true},
		{"zero version", []string{"0000_a.sql"}, true},
		{"non-sql files ignored", []string{"0001_a.sql", "README.md"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadMigrations(fsOf(tc.files...))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

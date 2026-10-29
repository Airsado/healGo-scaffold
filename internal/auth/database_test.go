package auth

import (
	"testing"

	"heago-scaffold/internal/testutil"

	"gorm.io/gorm"
)

// The shared helper isolates real databases; each package still registers its
// own models explicitly so importing test utilities never has schema side effects.
func isolatedDB(t *testing.T) *gorm.DB {
	t.Helper()
	database := testutil.OpenDatabase(t)
	if err := database.AutoMigrate(Models()...); err != nil {
		t.Fatal("initialize auth models")
	}
	return database
}

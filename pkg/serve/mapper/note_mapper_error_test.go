package mapper

import (
	"context"
	"errors"
	"strings"
	"testing"

	model "heago-scaffold/internal/model/note"
)

type duplicateError struct{}

func (duplicateError) Error() string    { return "private submitted title in PostgreSQL detail" }
func (duplicateError) SQLState() string { return "23505" }

func TestDriverErrorsAreClassifiedWithoutLeakingDetails(t *testing.T) {
	for _, input := range []error{errors.New("private database DSN and values"), duplicateError{}} {
		err := safeError(context.Background(), input)
		if strings.Contains(err.Error(), "private") {
			t.Fatal("raw database detail escaped mapper")
		}
	}
	if err := safeError(context.Background(), duplicateError{}); !errors.Is(err, model.ErrConflict) {
		t.Fatal("untranslated PostgreSQL duplicate was not classified")
	}
}

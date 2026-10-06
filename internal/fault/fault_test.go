package fault

import (
	"errors"
	"fmt"
	"testing"
)

func TestInvalidKeepsTheMessageAndSurvivesWrapping(t *testing.T) {
	base := errors.New("unknown app \"web\"")
	marked := Invalid(base)
	if marked.Error() != base.Error() || !errors.Is(marked, base) || !IsInvalid(marked) {
		t.Fatalf("marked = %v", marked)
	}
	if wrapped := fmt.Errorf("start: %w", marked); !IsInvalid(wrapped) {
		t.Fatal("a wrapped mark was lost")
	}
	if IsInvalid(base) || IsInvalid(nil) || Invalid(nil) != nil {
		t.Fatal("an unmarked error counts as invalid")
	}
	if err := Invalidf("bad %w", base); !IsInvalid(err) || !errors.Is(err, base) {
		t.Fatalf("Invalidf = %v", err)
	}
}

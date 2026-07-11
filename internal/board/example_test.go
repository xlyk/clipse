package board

import "testing"

// TestExampleSpecParsesAndValidates guards against drift between the shipped
// configs/board.example.yaml, its body_file, and the parser/validator.
func TestExampleSpecParsesAndValidates(t *testing.T) {
	spec, err := Parse("../../configs/board.example.yaml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := spec.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

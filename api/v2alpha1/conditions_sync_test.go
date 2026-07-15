package v2alpha1

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestConditionsFileMirrorsV2Beta1 pins the Q309 acceptance invariant: the v2
// condition vocabulary lives in two files — this package's conditions.go and
// api/v2beta1/conditions.go — that must stay byte-identical except for the
// package clause. There is no generator; whoever edits one mirrors the other
// (e.g. `awk 'NR==1{print "package v2beta1"; next} {print}' v2alpha1/conditions.go
// > v2beta1/conditions.go`), and this test catches the miss.
func TestConditionsFileMirrorsV2Beta1(t *testing.T) {
	alpha, err := os.ReadFile("conditions.go")
	if err != nil {
		t.Fatalf("read v2alpha1/conditions.go: %v", err)
	}
	beta, err := os.ReadFile(filepath.Join("..", "v2beta1", "conditions.go"))
	if err != nil {
		t.Fatalf("read v2beta1/conditions.go: %v", err)
	}

	alphaBody, ok := bytes.CutPrefix(alpha, []byte("package v2alpha1\n"))
	if !ok {
		t.Fatal("v2alpha1/conditions.go must start with its package clause on line 1")
	}
	betaBody, ok := bytes.CutPrefix(beta, []byte("package v2beta1\n"))
	if !ok {
		t.Fatal("v2beta1/conditions.go must start with its package clause on line 1")
	}
	if !bytes.Equal(alphaBody, betaBody) {
		t.Fatal("api/v2alpha1/conditions.go and api/v2beta1/conditions.go have diverged; they must stay byte-identical except for the package clause — mirror the edit into both files")
	}
}

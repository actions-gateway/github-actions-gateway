package controller

import (
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"

	agcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
)

// errRESTMapper is a meta.RESTMapper whose RESTMapping always returns a
// non-NoMatch error, used to prove RunnerSetInstalled surfaces real discovery
// failures rather than treating them as "v2 absent". Embedding the interface
// supplies the other (unused) methods.
type errRESTMapper struct {
	meta.RESTMapper
	err error
}

func (m errRESTMapper) RESTMapping(_ schema.GroupKind, _ ...string) (*meta.RESTMapping, error) {
	return nil, m.err
}

func TestRunnerSetInstalled_Absent(t *testing.T) {
	// A DefaultRESTMapper with no kinds registered returns a NoMatch for every
	// lookup — the v1-only install state.
	mapper := meta.NewDefaultRESTMapper(nil)

	installed, err := RunnerSetInstalled(mapper)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if installed {
		t.Fatal("expected RunnerSetInstalled to report false when the RunnerSet kind is not mapped")
	}
}

func TestRunnerSetInstalled_Present(t *testing.T) {
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{agcv2alpha1.GroupVersion})
	mapper.Add(agcv2alpha1.GroupVersion.WithKind("RunnerSet"), meta.RESTScopeNamespace)

	installed, err := RunnerSetInstalled(mapper)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !installed {
		t.Fatal("expected RunnerSetInstalled to report true when the RunnerSet kind is mapped")
	}
}

func TestRunnerSetInstalled_DiscoveryErrorPropagates(t *testing.T) {
	sentinel := errors.New("apiserver discovery boom")
	mapper := errRESTMapper{err: sentinel}

	installed, err := RunnerSetInstalled(mapper)
	if err == nil {
		t.Fatal("expected a non-NoMatch discovery error to propagate")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected wrapped sentinel error, got: %v", err)
	}
	if installed {
		t.Fatal("expected installed=false on discovery error")
	}
}

package controller

import (
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
)

// errRESTMapper is a meta.RESTMapper whose RESTMapping always returns a
// non-NoMatch error, used to prove V2alpha1Installed surfaces real discovery
// failures rather than treating them as "v2 absent". Embedding the interface
// supplies the other (unused) methods.
type errRESTMapper struct {
	meta.RESTMapper
	err error
}

func (m errRESTMapper) RESTMapping(_ schema.GroupKind, _ ...string) (*meta.RESTMapping, error) {
	return nil, m.err
}

func TestV2alpha1Installed_Absent(t *testing.T) {
	// A DefaultRESTMapper with no kinds registered returns a NoMatch for every
	// lookup — the v1-only install state.
	mapper := meta.NewDefaultRESTMapper(nil)

	installed, err := V2alpha1Installed(mapper)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if installed {
		t.Fatal("expected V2alpha1Installed to report false when no v2 kinds are mapped")
	}
}

func TestV2alpha1Installed_Present(t *testing.T) {
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{gmcv2alpha1.GroupVersion})
	for _, kind := range v2DetectKinds {
		mapper.Add(gmcv2alpha1.GroupVersion.WithKind(kind), meta.RESTScopeNamespace)
	}

	installed, err := V2alpha1Installed(mapper)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !installed {
		t.Fatal("expected V2alpha1Installed to report true when both v2 kinds are mapped")
	}
}

func TestV2alpha1Installed_PartialIsAbsent(t *testing.T) {
	// Only ActionsGateway mapped, EgressProxy missing: treat as not installed so a
	// half-applied CRD set never half-starts the v2 controllers.
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{gmcv2alpha1.GroupVersion})
	mapper.Add(gmcv2alpha1.GroupVersion.WithKind("ActionsGateway"), meta.RESTScopeNamespace)

	installed, err := V2alpha1Installed(mapper)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if installed {
		t.Fatal("expected V2alpha1Installed to report false when only some v2 kinds are mapped")
	}
}

func TestV2alpha1Installed_DiscoveryErrorPropagates(t *testing.T) {
	sentinel := errors.New("apiserver discovery boom")
	mapper := errRESTMapper{err: sentinel}

	installed, err := V2alpha1Installed(mapper)
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

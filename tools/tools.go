//go:build tools

package tools

import (
	_ "github.com/elastic/crd-ref-docs"
	_ "github.com/golangci/golangci-lint/v2/cmd/golangci-lint"
	_ "github.com/jbeda/mdreflow/cmd/mdreflow"
	_ "github.com/rhysd/actionlint/cmd/actionlint"
	_ "golang.org/x/vuln/cmd/govulncheck"
	_ "sigs.k8s.io/controller-runtime/tools/setup-envtest"
	_ "sigs.k8s.io/controller-tools/cmd/controller-gen"
	_ "sigs.k8s.io/kubebuilder/v4"
)

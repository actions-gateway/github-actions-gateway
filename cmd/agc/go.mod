module github.com/karlkfi/github-actions-gateway/agc

go 1.22

require (
	github.com/prometheus/client_golang v1.19.1
	github.com/stretchr/testify v1.9.0
	go.uber.org/goleak v1.3.0
	k8s.io/api v0.30.0
	k8s.io/apimachinery v0.30.0
	k8s.io/client-go v0.30.0
	sigs.k8s.io/controller-runtime v0.18.4
)

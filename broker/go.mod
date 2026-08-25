module github.com/actions-gateway/github-actions-gateway/broker

go 1.26.6

require (
	github.com/actions-gateway/github-actions-gateway/githubapp v0.0.0-00010101000000-000000000000
	github.com/stretchr/testify v1.12.1
	go.uber.org/goleak v1.3.0
)

require (
	github.com/golang-jwt/jwt/v5 v5.3.1 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
)

replace github.com/actions-gateway/github-actions-gateway/githubapp => ../githubapp

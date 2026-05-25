module github.com/karlkfi/github-actions-gateway/broker

go 1.26

replace github.com/karlkfi/github-actions-gateway/githubapp => ../githubapp

require (
	github.com/karlkfi/github-actions-gateway/githubapp v0.0.0-00010101000000-000000000000
	github.com/stretchr/testify v1.11.1
	go.uber.org/goleak v1.3.0
)

require (
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.1 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

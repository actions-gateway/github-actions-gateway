module github.com/karlkfi/github-actions-gateway/probe

go 1.26

require (
	github.com/karlkfi/github-actions-gateway/broker v0.0.0-00010101000000-000000000000
	github.com/karlkfi/github-actions-gateway/githubapp v0.0.0-00010101000000-000000000000
)

require github.com/golang-jwt/jwt/v5 v5.3.1 // indirect

replace github.com/karlkfi/github-actions-gateway/broker => ../../broker

replace github.com/karlkfi/github-actions-gateway/githubapp => ../../githubapp

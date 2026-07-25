module github.com/actions-gateway/github-actions-gateway/test/fakegithub

go 1.26.5

require github.com/actions-gateway/github-actions-gateway/broker v0.0.0-00010101000000-000000000000

replace github.com/actions-gateway/github-actions-gateway/broker => ../../broker

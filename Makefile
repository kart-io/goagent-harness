.PHONY: test race build verify-local

test:
	GOWORK=off go test ./...

race:
	GOWORK=off go test -race ./...

build:
	GOWORK=off go build ./...

verify-local:
	./scripts/verify-goagent.sh

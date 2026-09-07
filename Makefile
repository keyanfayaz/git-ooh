.PHONY: build check dev test web

web:
	pnpm --dir web build

build: web
	GOCACHE=/tmp/git-ooh-go-cache go build -o bin/ooh ./cmd/ooh

dev:
	pnpm --dir web dev

test:
	GOCACHE=/tmp/git-ooh-go-cache go test ./...
	pnpm --dir web test -- --run

check:
	@test -z "$$(gofmt -l cmd internal skills/embed.go)" || (gofmt -l cmd internal skills/embed.go && exit 1)
	GOCACHE=/tmp/git-ooh-go-cache go vet ./...
	pnpm --dir web check

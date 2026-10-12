set dotenv-load := true

default:
    @just --list

build:
    mkdir -p bin
    go build -o bin/sail-lite ./cmd/sail-lite
    GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o bin/saild-linux ./cmd/saild
    GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o bin/guest-agent-linux ./cmd/guest-agent

install:
    go install ./cmd/sail-lite

test:
    go test -race ./...

format:
    gofmt -w api cmd internal tests

generate:
    buf generate

provision:
    python3 scripts/provision.py

bootstrap:
    python3 scripts/deploy.py bootstrap

redeploy:
    python3 scripts/deploy.py redeploy

tunnel:
    python3 scripts/deploy.py tunnel

ssh:
    python3 scripts/deploy.py ssh

destroy:
    cd infra && pulumi destroy --yes

smoke:
    go test -tags smoke ./tests -v -count=1

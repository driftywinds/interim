.PHONY: build test vet run docker

build:
	CGO_ENABLED=0 go build -trimpath -o bin/interim ./cmd/interim

test:
	go test ./...

vet:
	go vet ./...

run:
	go run ./cmd/interim

docker:
	docker compose up --build

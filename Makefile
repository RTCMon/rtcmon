.PHONY: build build-api clean lint test test-short tidy migrate-up migrate-down migrate-create

build:
	go build ./...

build-api:
	mkdir -p bin
	go build -o bin/ingest-api ./services/ingest-api

clean:
	rm -rf bin/

lint:
	golangci-lint run ./...

test:
	go test -race ./...

test-short:
	go test -short -race ./...

tidy:
	go mod tidy

migrate-up:
	go run ./services/ingest-api migrate up

migrate-down:
	go run ./services/ingest-api migrate down

# Usage: make migrate-create name=add_something
migrate-create:
	migrate create -ext sql -dir migrations -seq $(name)

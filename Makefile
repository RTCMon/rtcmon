.PHONY: build build-api clean lint test test-short test-unit test-integration coverage tidy swagger migrate-up migrate-down migrate-create

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

test-unit:
	go test -short -race ./...

test-integration:
	go test -tags integration -race -timeout 5m ./...

coverage:
	go test -tags integration -coverprofile=coverage.out -coverpkg=./internal/... -timeout 5m ./...
	go tool cover -func=coverage.out | grep total

tidy:
	go mod tidy

swagger:
	swag init --dir ./services/query-api -g main.go -o services/query-api/docs
	swag init --dir ./services/ingest-api -g main.go -o services/ingest-api/docs

migrate-up:
	go run ./services/ingest-api migrate up

migrate-down:
	go run ./services/ingest-api migrate down

# Usage: make migrate-create name=add_something
migrate-create:
	migrate create -ext sql -dir migrations -seq $(name)

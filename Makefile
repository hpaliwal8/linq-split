.PHONY: build run test clean

build:
	go build -o bin/linq-split ./cmd/linq-split

run: build
	./bin/linq-split

test:
	go test ./... -v -count=1

clean:
	rm -rf bin/ linq-split.db

# Dev: run with auto-reload (requires air: go install github.com/air-verse/air@latest)
dev:
	air

# Expose local server for Linq webhooks (requires ngrok)
tunnel:
	ngrok http 8080

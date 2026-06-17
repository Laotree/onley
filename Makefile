BINARY  := onley
CMD     := ./cmd/onley
GOFLAGS := -trimpath

.PHONY: all build test coverage lint clean

all: build

build:
	go build $(GOFLAGS) -o $(BINARY) $(CMD)

test:
	go test ./... -count=1

coverage:
	go test ./... -count=1 -coverprofile=coverage.out
	go tool cover -func=coverage.out
	go tool cover -html=coverage.out -o coverage.html

lint:
	golangci-lint run ./...

clean:
	rm -f $(BINARY) coverage.out coverage.html

BINARY := jsq
PKG    := .

.PHONY: all build run clean test tidy

all: build

## build: compile a stripped, optimized production binary
build:
	CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags "-s -w" -o $(BINARY) $(PKG)

## run: build and run the app (pass args with ARGS="...")
run:
	go run $(PKG) $(ARGS)

## test: run the test suite
test:
	go test ./...

## tidy: sync go.mod/go.sum
tidy:
	go mod tidy

## clean: remove build artifacts
clean:
	rm -f $(BINARY)

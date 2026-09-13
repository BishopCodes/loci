BIN  := bin/dsearch
GO   ?= go

.PHONY: build test vet fmt doctor clean

build:
	$(GO) build -o $(BIN) ./cmd/dsearch

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -w $(shell gofmt -l .)

doctor: build
	$(BIN) doctor

clean:
	rm -rf bin .probe .smoke

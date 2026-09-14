BIN  := bin/loci
GO   ?= go
PREFIX ?= $(HOME)/.local

.PHONY: build test vet fmt doctor install clean

build:
	$(GO) build -o $(BIN) ./cmd/loci

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	@files=$$(gofmt -l .); test -z "$$files" || gofmt -w $$files

doctor: build
	$(BIN) doctor

install: build
	install -Dm755 $(BIN) $(DESTDIR)$(PREFIX)/bin/$(notdir $(BIN))

clean:
	rm -rf bin .probe .smoke

.PHONY: build build-all test e2e vet clean

BINARY = plugin
PLATFORMS = linux/amd64 linux/arm64 darwin/arm64
# manifest.json's version, which a release tag must match.
VERSION ?= $(shell jq -r .version manifest.json)
LDFLAGS = -s -w -X main.version=$(VERSION)

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o $(BINARY) .

build-all:
	@mkdir -p dist
	@for platform in $(PLATFORMS); do \
		GOOS=$${platform%%/*} GOARCH=$${platform##*/} CGO_ENABLED=0 \
		go build -trimpath -ldflags="$(LDFLAGS)" -o dist/$(BINARY)-$${platform%%/*}-$${platform##*/} . || exit 1; \
	done

test:
	go test ./...

e2e:
	go test -tags integration ./...

vet:
	test -z "$$(gofmt -l .)"
	go vet -tags integration ./...

clean:
	rm -rf $(BINARY) dist

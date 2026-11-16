GO ?= go
.PHONY: test release check
test:
	$(GO) test ./...
check:
	$(GO) vet ./...
	$(GO) test -race ./...
release:
	mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=$(or $(ARCH),amd64) $(GO) build -mod=readonly -buildvcs=false -trimpath -ldflags="-s -w" -o bin/social-mcp ./cmd/server
	sha256sum bin/social-mcp > bin/social-mcp.sha256

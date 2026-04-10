install-tools: ## Install tools
	@go install github.com/rhysd/actionlint/cmd/actionlint@v1.7.7
	@go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.5.0

check-tool-%:
	@which $* > /dev/null || (echo "Install $* with 'make install-tools'"; exit 1 )

lint: check-tool-golangci-lint
	@actionlint
	@golangci-lint run ./...

test:
	@go test ./...

.PHONY: test

generate:
	@go run ./cmd/pin-gha local-repository .

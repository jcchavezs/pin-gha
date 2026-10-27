.PHONY: install-tools
install-tools: ## Install tools
	@go install github.com/rhysd/actionlint/cmd/actionlint@v1.7.7
	@go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.5.0

check-tool-%:
	@which $* > /dev/null || (echo "Install $* with 'make install-tools'"; exit 1 )

.PHONY: lint
lint: check-tool-golangci-lint ## Run linters
	@actionlint
	@golangci-lint run ./...

.PHONY: test
test: ## Run tests
	@go test ./...

.PHONY: generate
generate: ## Generate code
	@go run ./cmd/pin-gha local-repository .

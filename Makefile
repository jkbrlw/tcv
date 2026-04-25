.PHONY: build install build-base build-proxy build-images clean help

build: ## Build the tcv CLI
	cd cli && go build -o tcv .
	@echo "Built cli/tcv"

install: build ## Install CLI to /usr/local/bin
	@# Remove first to avoid "Text file busy" if running
	sudo rm -f /usr/local/bin/tcv
	sudo cp cli/tcv /usr/local/bin/tcv
	@echo "Installed tcv to /usr/local/bin"

build-base: install ## Build the base agent container image (requires tcv installed)
	tcv build tcv-agent-base

build-proxy: install ## Build the egress proxy container image (requires tcv installed)
	tcv build tcv-egress

build-images: install ## Build all container images (requires tcv installed)
	tcv build --all

clean: ## Remove build artifacts
	rm -f cli/tcv

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

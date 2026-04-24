.PHONY: build install build-base build-proxy build-images clean

build: ## Build the tcv CLI
	cd cli && go build -o tcv .
	@echo "Built cli/tcv"

install: build ## Install CLI to /usr/local/bin
	@# Remove first to avoid "Text file busy" if running
	sudo rm -f /usr/local/bin/tcv
	sudo cp cli/tcv /usr/local/bin/tcv
	@echo "Installed tcv to /usr/local/bin"

build-base: ## Build the base agent container image
	tcv build tcv-agent-base

build-proxy: ## Build the egress proxy container image
	tcv build tcv-egress

build-images: ## Build all container images (base + proxy + custom)
	tcv build --all

clean: ## Remove build artifacts
	rm -f cli/tcv

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

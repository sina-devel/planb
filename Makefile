IMG ?= ghcr.io/sina-devel/planb:dev
CONTROLLER_GEN_VERSION ?= v0.23.0
# Uses controller-gen from PATH (nix-shell provides it), else runs it via go.
CONTROLLER_GEN ?= $(or $(shell command -v controller-gen 2>/dev/null),go run sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION))

.PHONY: all
all: generate build

.PHONY: tidy
tidy: ## Resolve Go dependencies
	go mod tidy

.PHONY: generate
generate: ## Generate deepcopy code, CRDs and RBAC
	$(CONTROLLER_GEN) object paths=./api/...
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:artifacts:config=charts/planb/crds
	$(CONTROLLER_GEN) rbac:roleName=planb paths=./internal/controller/... output:rbac:artifacts:config=hack/rbac

.PHONY: fmt
fmt:
	go fmt ./...

.PHONY: vet
vet:
	go vet ./...

.PHONY: test
test: vet
	go test ./...

.PHONY: build
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/planb ./cmd/planb

.PHONY: run
run: ## Run the manager against the current kubeconfig
	go run ./cmd/planb manager --agent-image=$(IMG)

.PHONY: run-ui
run-ui: ## Run the UI against the current kubeconfig on :8090
	go run ./cmd/planb ui

.PHONY: docker-build
docker-build:
	docker build -t $(IMG) .

.PHONY: docker-push
docker-push:
	docker push $(IMG)

.PHONY: install
install: ## Install CRDs into the current cluster
	kubectl apply --server-side -f charts/planb/crds

.PHONY: deploy
deploy: ## Install the Helm chart
	helm upgrade --install planb charts/planb -n planb-system --create-namespace \
		--set image.repository=$(firstword $(subst :, ,$(IMG))) --set image.tag=$(lastword $(subst :, ,$(IMG)))

.PHONY: e2e
e2e: ## Run the agent against a local PostgreSQL server and rclone S3 endpoint
	./hack/e2e-local.sh

SHELL := /bin/bash

BIN ?= bin/manager
PKG := virtops-operator

.PHONY: all build run tidy clean apply-manifests

all: build

build:
	@echo "Building $(PKG) ..."
	@mkdir -p bin
	GO111MODULE=on go build -o $(BIN) ./main.go

run:
	@echo "Running $(PKG) in dev mode (uses current kubeconfig) ..."
	GO111MODULE=on go run ./main.go

 tidy:
	@echo "Running go mod tidy ..."
	go mod tidy

clean:
	@echo "Cleaning ..."
	rm -rf bin

apply-manifests:
	@echo "Applying minimal RBAC and CRD into cluster ..."
	kubectl apply -f config/rbac/service_account.yaml
	kubectl apply -f config/rbac/role.yaml
	kubectl apply -f config/rbac/role_binding.yaml
	kubectl apply -f config/crd/bases/admincredentialrotationpolicies.guestops.io.crd.yaml

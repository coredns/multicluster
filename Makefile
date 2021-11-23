IMAGE_REPOSITORY ?= ghcr.io/aws/aws-cloud-map-mcs-controller-for-k8s
IMAGE_VERSION ?= v1.8.6
GOLANGCI_VERSION ?= 1.43.0
SOURCE_DIRS = . cmd

OUTDIR	:= bin

.PHONY: test pretty mod tidy build
test:
	CGO_ENABLED=0 go test $(shell go list ./... | grep -v /vendor/|xargs echo) -cover -coverprofile=cover.out

mod:
	go mod download

tidy:
	go mod tidy

pretty:
	gofmt -l -s -w $(SOURCE_DIRS)

bin/golangci-lint: bin/golangci-lint-${GOLANGCI_VERSION}
	@ln -sf golangci-lint-${GOLANGCI_VERSION} bin/golangci-lint
bin/golangci-lint-${GOLANGCI_VERSION}:
	@mkdir -p bin
	curl -sfL https://install.goreleaser.com/github.com/golangci/golangci-lint.sh | bash -s -- -b ./bin/ v${GOLANGCI_VERSION}
	@mv bin/golangci-lint "$@"

.PHONY: lint
lint: bin/golangci-lint ## Run linter
	bin/golangci-lint run

build:
	CGO_ENABLED=0 go build -o coredns -ldflags="-s -w" cmd/coredns/main.go

run:
	go run cmd/coredns/main.go -conf corefile.example -dns.port=10053

.PHONY: dist
dist:
	mkdir -p $(OUTDIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -v -o $(OUTDIR)/coredns -ldflags="-s -w" cmd/coredns/main.go
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -v -o $(OUTDIR)/coredns-darwin -ldflags="-s -w" cmd/coredns/main.go

package: dist docker-build docker-push

docker-build:
	docker build --no-cache -t $(IMAGE_REPOSITORY)/coredns-multicluster:$(IMAGE_VERSION) .

docker-push:
	docker push $(IMAGE_REPOSITORY)/coredns-multicluster:$(IMAGE_VERSION)

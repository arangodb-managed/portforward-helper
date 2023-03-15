SHELL = bash
PROJECT := portforward-helper
ROOTDIR := $(shell pwd)

.PHONY: all
all: check test

.PHONY: check
check:
	zutano go check ./...
	zutano check project --quiet

.PHONY: test
test:
	mkdir -p bin/test
	go test -coverprofile=bin/test/coverage.out -v ./... | tee bin/test/test-output.txt ; exit "$${PIPESTATUS[0]}"
	cat bin/test/test-output.txt | go-junit-report > bin/test/unit-tests.xml
	go tool cover -html=bin/test/coverage.out -o bin/test/coverage.html

bootstrap:
	go get github.com/arangodb-managed/zutano
	go get github.com/jstemmer/go-junit-report

.PHONY: update-modules
update-modules:
	zutano update-check --quiet --fail
	test -f go.mod || go mod init
	go mod edit \
		$(shell zutano go mod replacements)
	go get \
		$(shell zutano go mod latest \
			github.com/arangodb-managed/apis \
			github.com/arangodb-managed/apis-helper \
			github.com/arangodb-managed/arangodb-helper \
			github.com/arangodb-managed/internal-apis \
			github.com/arangodb-managed/log-helper \
			github.com/arangodb-managed/metrics-helper \
			github.com/arangodb-managed/refs-helper \
			)
	go mod tidy

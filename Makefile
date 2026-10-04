# Local development targets. `build` produces a release-style shared library
# for the host platform; CI builds every supported platform.
.PHONY: test lint vet build clean

test:
	go test -race ./...

vet:
	go vet ./...

lint:
	golangci-lint run ./...

build:
	scripts/build-plugin.sh dist

clean:
	rm -rf dist

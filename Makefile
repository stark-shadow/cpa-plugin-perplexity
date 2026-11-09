.PHONY: build test package clean

build:
	bash build.sh

test:
	go test -race -v ./...

package: build

clean:
	rm -rf bin dist .build

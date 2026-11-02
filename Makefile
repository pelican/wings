GIT_HEAD = $(shell git rev-parse HEAD | head -c8)

build:
	GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -gcflags "all=-trimpath=$(CURDIR)" -o build/wings_linux_amd64 -v wings.go
	GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -gcflags "all=-trimpath=$(CURDIR)" -o build/wings_linux_arm64 -v wings.go

test:
	go test -race ./...

# Regenerate the symbol table the plugin interpreter resolves imports against.
# Run this after changing anything in plugins/api, or interpreted plugins will
# still be compiled against the previous contract. The symbols test fails and
# names what is missing when this is out of date.
plugin-symbols:
	cd plugins && rm -f github_com-pelican-wings-plugins-api.go && \
		go run github.com/traefik/yaegi/cmd/yaegi extract github.com/pelican/wings/plugins/api
	gofmt -w plugins/

debug:
	go build -ldflags="-X github.com/pelican/wings/system.Version=$(GIT_HEAD)"
	sudo ./wings --debug --ignore-certificate-errors --config config.yml --pprof --pprof-block-rate 1

# Runs a remotly debuggable session for Wings allowing an IDE to connect and target
# different breakpoints.
rmdebug:
	go build -gcflags "all=-N -l" -ldflags="-X github.com/pelican/wings/system.Version=$(GIT_HEAD)" -race
	sudo dlv --listen=:2345 --headless=true --api-version=2 --accept-multiclient exec ./wings -- --debug --ignore-certificate-errors --config config.yml

build-darwin:
	GOOS=darwin GOARCH=arm64 go build -ldflags="-s -w" -gcflags "all=-trimpath=$(CURDIR)" -o build/wings_darwin_arm64 -v wings.go
	GOOS=darwin GOARCH=amd64 go build -ldflags="-s -w" -gcflags "all=-trimpath=$(CURDIR)" -o build/wings_darwin_amd64 -v wings.go

cross-build: clean build build-darwin

clean:
	rm -rf build/wings_*

.PHONY: build build-darwin cross-build clean test plugin-symbols debug rmdebug

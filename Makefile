BIN      := localaws
LDFLAGS  := -s -w
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

.PHONY: build test release image clean console console-dev console-test

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) .

test:
	go vet ./... && go test -count=1 ./...

release:
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=$$( [ $$os = windows ] && echo .exe ); \
		echo "dist/$(BIN)-$$os-$$arch$$ext"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BIN)-$$os-$$arch$$ext . || exit 1; \
	done

image:
	docker build -t $(BIN):latest .

clean:
	rm -rf dist $(BIN) $(BIN).exe data

# the web console (React) — its build is committed in console/dist and embedded by go build
console:
	cd console && npm ci && npm run build

console-dev:            # hot reload on :5173, API proxied to LOCALAWS_URL (default http://localhost:4566)
	cd console && npm run dev

console-test:           # drives the console in the installed Chrome against LOCALAWS_URL
	cd console && node e2e/ui-smoke.mjs

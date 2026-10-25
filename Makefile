LDFLAGS := -s -w
PLATFORMS := linux/amd64 linux/arm64 windows/amd64 darwin/amd64 darwin/arm64

.PHONY: build dist clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o warren ./cmd/warren

# Binaries for every platform in dist/, ready to hand to friends.
dist:
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		echo "building $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" \
			-o dist/warren-$$os-$$arch$$ext ./cmd/warren || exit 1; \
	done

clean:
	rm -rf warren dist

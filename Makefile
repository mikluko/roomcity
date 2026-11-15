.PHONY: generate lint test image

# Regenerates internal/gen from proto/. The plugins are the module's tools
# (go.mod), so buf runs the versions go.sum pins.
generate:
	buf generate

lint:
	buf lint
	buf format --diff --exit-code
	go vet ./...

test:
	go test ./...

# Builds the image through its smoke stage, which runs the image's tools.
image:
	container build --target smoke --progress plain .

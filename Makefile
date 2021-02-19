.PHONY: default
default: gen
	go build -o _output/session-gate ./cmd

.PHONY: gen
gen:
	go generate ./...

.PHONY: image
image: gen
	kubectl dev build -t docker.io/warmmetal/session-gate:v0.2.0
all:
	go generate ./...
	kubectl dev build -f hack/session-gate/Dockerfile -t docker.io/warmmetal/session-gate:v0.1.0
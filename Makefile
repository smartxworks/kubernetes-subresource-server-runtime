fmt:
	go fmt ./...

test:
	go test -coverprofile=cover.out ./...

run:
	skaffold run --tail

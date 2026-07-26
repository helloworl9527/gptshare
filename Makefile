.PHONY: test release

test:
	go test ./...

release:
	./scripts/build-release.sh

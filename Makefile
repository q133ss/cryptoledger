.PHONY: build test race vet integration bench demo proto
build:
	go build ./...
test:
	go test ./...
race:
	go test -race -count=1 ./...
vet:
	go vet ./...
integration:
	go test -race -count=1 -tags=integration ./...
bench:
	go test -run '^$$' -bench . -benchmem -cpu=4 -benchtime=5s -count=5 ./internal/ledger
demo:
	go run ./cmd/demo
proto:
	powershell -ExecutionPolicy Bypass -File scripts/gen.ps1

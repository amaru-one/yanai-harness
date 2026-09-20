BIN := yanai

.PHONY: build test fmt vet race demo clean

build:
	go build -o $(BIN) ./cmd/yanai

test:
	go test ./...

fmt:
	gofmt -w .

vet:
	go vet ./...

race:
	go test -race ./internal/workflow ./internal/team

# Runs through the full flow with mock responses.
demo: build
	rm -rf /tmp/yanai-demo && mkdir -p /tmp/yanai-demo
	./$(BIN) init --ws /tmp/yanai-demo/ws
	printf '# Entrevista\nLa docente pierde tiempo copiando notas al SIAGIE.\n' > /tmp/yanai-demo/ws/interviews/e1.md
	YANAI_MOCK=1 YANAI_NO_REPO=1 ./$(BIN) analyze --ws /tmp/yanai-demo/ws --privacy-reviewed /tmp/yanai-demo/ws/interviews/e1.md
	YANAI_MOCK=1 YANAI_NO_REPO=1 ./$(BIN) discuss --ws /tmp/yanai-demo/ws
	./$(BIN) approve --ws /tmp/yanai-demo/ws --note "demo"
	YANAI_MOCK=1 ./$(BIN) run --ws /tmp/yanai-demo/ws

clean:
	rm -f $(BIN)
	rm -rf /tmp/yanai-demo

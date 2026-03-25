BIN := trace-ui

.PHONY: build run clean

build:
	go build -o $(BIN) .

run: build
	./$(BIN)

run-dev: build
	./$(BIN) -host http://localhost:16686

clean:
	rm -f $(BIN)

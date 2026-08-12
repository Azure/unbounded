BIN := bin/memsnap
SRC := $(shell find . -name '*.go') go.mod go.sum

.PHONY: all clean run demo

all: $(BIN)

$(BIN): $(SRC)
	go build -o $@ .

run: $(BIN)
	sudo $(BIN)

demo: $(BIN)
	./demo.sh

clean:
	rm -rf bin

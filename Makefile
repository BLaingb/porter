BINARY    := porter
INSTALL   := /usr/local/bin

.PHONY: build install clean

build:
	go build -o $(BINARY) .

install: build
	cp $(BINARY) $(INSTALL)/$(BINARY)
	@echo "installed $(INSTALL)/$(BINARY)"

clean:
	rm -f $(BINARY)

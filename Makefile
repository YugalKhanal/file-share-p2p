# build:
# 	@go build -o bin/fs
#
# run: build
# 	@./bin/fs
#
# test:
# 	@go test ./...


build:
	@go build -o bin/fs

run: build
	@./bin/fs $(ARGS)

test:
	@go test ./...

.PHONY: test-network
test-network:
	./test_network.sh

.PHONY: clean
clean:
	rm -f downloaded_* large_test.txt
	rm -rf shared_files/

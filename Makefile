build:
		@mkdir -p bin/linux-amd64
		@GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o ./bin/linux-amd64/worker ./cmd/worker/main.go
		@GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o ./bin/linux-amd64/driver ./cmd/driver/main.go

docker: build
		cp ./bin/linux-amd64/worker ./build/worker/worker
		cp ./bin/linux-amd64/driver ./build/driver/driver
		docker build -t Redacted-worker:test ./build/worker --progress=plain
		docker build -t Redacted-driver:test ./build/driver --progress=plain
		rm ./build/worker/worker
		rm ./build/driver/driver
proto:
		protoc -I=$(PWD) --go_out=$(PWD) --go-grpc_out=$(PWD)  $(PWD)/proto/*.proto

.PHONY: build docker proto
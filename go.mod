module github.com/AbhinavSingh95/mica-mesh

go 1.26.8

require (
	github.com/google/uuid v1.6.0
	golang.org/x/sys v0.47.0
	google.golang.org/grpc v1.71.0
	google.golang.org/grpc/cmd/protoc-gen-go-grpc v1.5.1
	google.golang.org/protobuf v1.36.10
)

require (
	github.com/libp2p/zeroconf/v2 v2.2.0
	github.com/miekg/dns v1.1.43 // indirect
	golang.org/x/net v0.34.0 // indirect
	golang.org/x/text v0.21.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20250115164207-1a7da9e5054f // indirect
)

tool (
	google.golang.org/grpc/cmd/protoc-gen-go-grpc
	google.golang.org/protobuf/cmd/protoc-gen-go
)

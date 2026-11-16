module sarak.sh/mikluko/roomcity

go 1.27.4

tool (
	connectrpc.com/connect/cmd/protoc-gen-connect-go
	google.golang.org/protobuf/cmd/protoc-gen-go
)

require (
	connectrpc.com/connect v1.21.1
	github.com/afrase/acp-go v0.2.0
	github.com/stretchr/testify v1.12.1
	google.golang.org/protobuf v1.36.12
	sarak.sh/mikluko/claudeacp v0.6.0
)

require go.yaml.in/yaml/v3 v3.0.5 // indirect

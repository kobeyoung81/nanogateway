module gitee.com/kobeyoung81/nanogateway

go 1.24.0

require (
	gitee.com/kobeyoung81/nanoprotocol v0.43.0
	github.com/golang-jwt/jwt/v5 v5.2.1
	github.com/google/uuid v1.6.0
	github.com/gorilla/websocket v1.5.3
	github.com/larksuite/oapi-sdk-go/v3 v3.5.3
	github.com/spf13/cobra v1.8.1
	github.com/xen0n/go-workwx/v2 v2.0.0-alpha.1
)

require (
	github.com/cenkalti/backoff/v4 v4.3.0 // indirect
	github.com/gogo/protobuf v1.3.2 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/spf13/pflag v1.0.5 // indirect
)

replace github.com/xen0n/go-workwx/v2 v2.0.0-alpha.1 => ./go-workwx

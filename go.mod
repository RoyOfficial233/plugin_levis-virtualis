module github.com/SakuraOpenSource/plugin_levis-virtualis

go 1.26.4

toolchain go1.27.2

require (
	github.com/SakuraOpenSource/levis v0.0.0
	google.golang.org/grpc v1.83.2
)

require (
	golang.org/x/net v0.60.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)

replace github.com/SakuraOpenSource/levis => ../levis

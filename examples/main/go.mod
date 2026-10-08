module github.com/Gameye/nakama-fleetmanager/examples/main

go 1.27.1

// The example builds against the library in this repository.
replace github.com/Gameye/nakama-fleetmanager => ../..

require (
	github.com/Gameye/nakama-fleetmanager v0.0.0-00010101000000-000000000000
	// Must match the versions Nakama 3.41.0 is built with.
	github.com/heroiclabs/nakama-common v1.48.0
)

require (
	github.com/apapsch/go-jsonmerge/v2 v2.0.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/oapi-codegen/oapi-codegen/v2 v2.4.1 // indirect
	github.com/oapi-codegen/runtime v1.1.1 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)

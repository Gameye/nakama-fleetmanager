# Nakama And Gameye Integration

## Development

```sh
$ go version
go version go1.23.5 linux/amd64
```

```sh
go install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@latest
```

```sh
docker compose build && docker compose up -d
```

### Downloading Dependencies

```sh
go get -v ./...
```

```sh
rm -rf vendor && go mod vendor
```

## Regenerating OpenAPI Code

```sh
oapi-codegen --config=api/openapi/client_config.yaml api/openapi/client.yaml
```
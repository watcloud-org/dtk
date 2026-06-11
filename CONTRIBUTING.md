# Contributing to dtk

## Requirements

- Go 1.24+
- Docker (for integration tests)

## Running tests

```sh
go test -v ./...
```

### Coverage report

```sh
./scripts/coverage.sh
# Open coverage.html in a browser
```

## Submitting changes

1. Fork the repository and create a branch from `main`.
2. Make your changes.
3. Ensure all tests pass: `go test ./...`
4. Open a pull request with a clear description of the change.

## Reporting issues

Open a GitHub issue with steps to reproduce, expected behavior, and actual behavior. Include dtk version and Docker version if applicable.

## Code style

Follow standard Go conventions (`gofmt`, `go vet`). Keep dependencies minimal — the project intentionally avoids external packages beyond `gopkg.in/yaml.v3`.

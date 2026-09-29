DYNAMODB_PORT ?= 8691
DYNAMODB_CONTAINER ?= dynago-dynamodb-local
export DYNAGO_TEST_ENDPOINT ?= http://127.0.0.1:$(DYNAMODB_PORT)

.PHONY: test test-unit generate dynamodb-up dynamodb-down lint

## test: run every test, including the examples against DynamoDB Local
test: dynamodb-up
	DYNAGO_REQUIRE_DB=1 go test ./...

## test-unit: run tests that need no DynamoDB (integration tests skip themselves)
test-unit:
	DYNAGO_TEST_ENDPOINT= go test ./...

## generate: regenerate the examples
generate:
	go generate ./examples/... ./internal/e2e/fixture/... ./internal/e2e/rekey/...

## dynamodb-up: start an in-memory DynamoDB Local for the tests
dynamodb-up:
	@docker inspect $(DYNAMODB_CONTAINER) >/dev/null 2>&1 || \
		docker run -d --name $(DYNAMODB_CONTAINER) -p 127.0.0.1:$(DYNAMODB_PORT):8000 \
			amazon/dynamodb-local:3.3.1 -jar DynamoDBLocal.jar -inMemory -sharedDb >/dev/null
	@docker start $(DYNAMODB_CONTAINER) >/dev/null
	@for i in $$(seq 60); do curl -s -o /dev/null http://127.0.0.1:$(DYNAMODB_PORT) && exit 0; sleep 0.5; done; \
		echo "DynamoDB Local did not start on port $(DYNAMODB_PORT)" >&2; exit 1

## dynamodb-down: remove the DynamoDB Local container
dynamodb-down:
	-docker rm -f $(DYNAMODB_CONTAINER)

GOLANGCI_LINT ?= go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0

## lint: gofmt, go vet and golangci-lint (.golangci.yml), over hand-written and generated code
lint:
	gofmt -l . | (! grep .)
	go vet ./...
	$(GOLANGCI_LINT) run ./...

# Fixture handler for shim_contract_test.go's TestRubyShim -- exercises the
# happy path and the exception path against a stub Runtime API.

def handler(event:, context:)
  { echoed: event, requestId: context.aws_request_id, functionName: context.function_name }
end

def throwing(event:, context:)
  raise ArgumentError, 'boom'
end

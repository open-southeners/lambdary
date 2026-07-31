# Fixture handler for shim_test.go's TestPythonShim -- exercises the happy
# path and the exception path against a stub Runtime API.


def handler(event, context):
    return {"echoed": event, "requestId": context.aws_request_id, "functionName": context.function_name}


def throwing(event, context):
    raise ValueError("boom")

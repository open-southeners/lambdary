def handler(event, context):
    return {
        "statusCode": 201,
        "headers": {"x-demo": "yes", "content-type": "text/plain"},
        "cookies": ["session=abc123"],
        "body": "created",
    }

from greeting import greet


def handler(event, context):
    return {"echo": event, "greeting": greet()}

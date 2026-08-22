import greeting


def lambda_handler(event, context):
    return {"echo": event, "message": greeting.MESSAGE}

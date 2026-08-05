"""POST /thumbnail?size=N  (defaults to THUMBNAIL_MAX_SIZE env var)

Send an image as the request body and get back a resized thumbnail,
same format, as a binary response. Exercises Lambdary's incoming AND
outgoing binary-body mapping, query-string parsing, and environment
variables together.
"""
import base64
import io
import os

from PIL import Image

DEFAULT_MAX_SIZE = int(os.environ.get("THUMBNAIL_MAX_SIZE", "200"))


def handler(event, context):
    body = event.get("body")
    if not body:
        return {
            "statusCode": 400,
            "body": "send a POST with an image as the body, e.g. curl --data-binary @photo.jpg",
        }

    raw = base64.b64decode(body) if event.get("isBase64Encoded") else body.encode("utf-8")

    query = event.get("queryStringParameters") or {}
    size_param = query.get("size")
    try:
        max_size = int(size_param) if size_param else DEFAULT_MAX_SIZE
    except ValueError:
        return {"statusCode": 400, "body": f"invalid size: {size_param!r}"}

    try:
        image = Image.open(io.BytesIO(raw))
        image.load()
    except Exception as exc:
        return {"statusCode": 400, "body": f"not a readable image: {exc}"}

    image_format = image.format or "PNG"
    image.thumbnail((max_size, max_size))

    out = io.BytesIO()
    image.save(out, format=image_format)

    return {
        "statusCode": 200,
        "headers": {"content-type": f"image/{image_format.lower()}"},
        "isBase64Encoded": True,
        "body": base64.b64encode(out.getvalue()).decode("ascii"),
    }

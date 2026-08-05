# thumbnail-python

Image-thumbnail service, built on Pillow. Exercises Lambdary's incoming
*and* outgoing binary-body mapping (a POST'd image in, a resized image
out), query strings, and `environment` config, in one handler.

## Setup

Lambdary doesn't install dependencies for you — like a real Lambda zip
deployment, whatever's in this directory is exactly what runs, for both
backends (bind-mounted as `/var/task` for the container backend, used
as-is for the process backend). Vendor `Pillow` flat, alongside
`lambda_function.py`:

```sh
pip install -r requirements.txt -t .
```

## Try it

From the `examples/` folder:

```sh
lambdary dev
curl --data-binary @photo.jpg "http://127.0.0.1:8000/thumbnail" -o thumb.jpg
curl --data-binary @photo.jpg "http://127.0.0.1:8000/thumbnail?size=64" -o thumb-small.jpg
```

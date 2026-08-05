# Function-owned Dockerfile: lambdary discovers this the same way it
# discovers a package.json or requirements.txt, and (see .lambda.yml,
# which sets neither `runtime` nor `local.backend`) builds and runs it on
# the container backend automatically — no --runtime flag, no base image
# to pick.
#
# lambdary can bind-mount a plain bootstrap file into a provided.* container
# with no Dockerfile at all, but that only works for an already-executable
# bootstrap — Go's has to be compiled from main.go first, and lambdary has
# no automatic build step for compiled runtimes. Building it in a throwaway
# stage here means nobody needs Go installed locally to run this example.
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod main.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -o /bootstrap .

FROM public.ecr.aws/lambda/provided:al2023
COPY --from=build /bootstrap /var/runtime/bootstrap
CMD ["bootstrap"]

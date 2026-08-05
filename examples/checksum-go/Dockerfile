# Function-owned Dockerfile: lambdary discovers this the same way it
# discovers a package.json or requirements.txt, and (see .lambda.yml,
# which sets neither `runtime` nor `local.backend`) builds and runs it on
# the container backend automatically — no --runtime flag, no base image
# to pick.
#
# AWS's public.ecr.aws/lambda/provided:al2023 base image ships no runtime
# of its own (unlike the nodejs/python images): its entrypoint always execs
# a fixed /var/runtime/bootstrap, so a custom-runtime function needs its
# compiled binary placed there. Building it in a throwaway stage means
# nobody needs Go installed locally to run this example.
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod main.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -o /bootstrap .

FROM public.ecr.aws/lambda/provided:al2023
COPY --from=build /bootstrap /var/runtime/bootstrap
CMD ["bootstrap"]

#!/usr/bin/env ruby
# Lambdary's dependency-free Ruby Runtime API shim — see
# plans/process-ruby-and-container-fallback.md's Unit A and DESIGN.md's
# "Process backend" section for why this exists instead of the official
# aws-lambda-ric gem (native extensions, per-function bundle install).
#
# Usage: ruby bootstrap.rb <handler>, where <handler> is AWS's own Ruby
# handler notation, e.g. "function.handler" or "source.MyModule::Handler.process":
# the FIRST "." splits the file to require from the rest, and the LAST "."
# of that rest splits an optional const path (a class/module name, which may
# itself contain "::") from the method name. "function.handler" requires
# "function" (from the function's own directory — spawn always sets cwd
# there, see plans/rie-darwin-spike.md) and calls the top-level method
# `handler`; "source.MyModule::Handler.process" requires "source" and calls
# `Object.const_get("MyModule::Handler").process`. AWS's Ruby handlers take
# keyword args: `handler(event:, context:)`.
#
# Runtime API loop (frozen, versioned API — see DESIGN.md's "Background"
# section), mirroring bootstrap.mjs/bootstrap.py one-for-one against the
# same endpoints:
#   GET  $AWS_LAMBDA_RUNTIME_API/2018-06-01/runtime/invocation/next
#   POST .../invocation/<id>/response   on a successful handler call
#   POST .../invocation/<id>/error      when the handler raises
#   POST .../runtime/init/error + exit 1   when the handler fails to load
#
# The RIE sets AWS_LAMBDA_RUNTIME_API on this process itself before spawning
# it (see plans/rie-darwin-spike.md); this shim only reads it.
#
# Targets Ruby >= 2.6 (macOS system ruby) syntax — no 3.x-only features.

require 'json'
require 'net/http'
require 'uri'

def post_json(url, body)
  uri = URI(url)

  Net::HTTP.start(uri.host, uri.port) do |http|
    request = Net::HTTP::Post.new(uri)
    request['Content-Type'] = 'application/json'
    request.body = JSON.generate(body)
    http.request(request)
  end
end

def error_payload(exc)
  {
    errorMessage: exc.message,
    errorType: exc.class.name,
    stackTrace: exc.backtrace || []
  }
end

# load_handler parses spec per AWS's Ruby handler notation (see the header
# comment above), requires the target file off $LOAD_PATH (with the
# function's cwd unshifted onto it, mirroring bootstrap.py's
# sys.path.insert(0, os.getcwd())), and returns a lambda taking the same
# keyword args (event:, context:) a real handler does.
def load_handler(spec)
  raise ArgumentError, "invalid handler #{spec.inspect}: want \"file.method\"" unless spec.include?('.')

  file, rest = spec.split('.', 2)
  raise ArgumentError, "invalid handler #{spec.inspect}: want \"file.method\"" if file.empty? || rest.empty?

  $LOAD_PATH.unshift(Dir.pwd)
  require file

  last_dot = rest.rindex('.')
  const_path = last_dot ? rest[0...last_dot] : nil
  method_name = last_dot ? rest[(last_dot + 1)..-1] : rest

  target = const_path ? Object.const_get(const_path) : self

  unless target.respond_to?(method_name, true)
    raise NoMethodError, "#{const_path || file} has no method #{method_name.inspect}"
  end

  ->(event:, context:) { target.send(method_name, event: event, context: context) }
end

# Context mirrors bootstrap.py's types.SimpleNamespace and bootstrap.mjs's
# plain object: aws_request_id, function_name, and
# get_remaining_time_in_millis (computed from the Lambda-Runtime-Deadline-Ms
# response header) — the same trio the other two shims expose.
Context = Struct.new(:aws_request_id, :function_name, :deadline_ms) do
  def get_remaining_time_in_millis
    deadline_ms ? deadline_ms - (Time.now.to_f * 1000).to_i : 0
  end
end

# next_invocation long-polls GET .../invocation/next, which the Runtime API
# deliberately leaves hanging for however long it takes the next invocation
# to arrive — for a local dev server, that's routinely minutes between
# manual test requests, unlike a warm production Lambda under steady
# traffic. Net::HTTP defaults read_timeout to 60s, which would raise
# Net::ReadTimeout and crash the whole runtime process on any idle gap
# longer than that, killing every later invocation until `dev` restarts it
# (the same failure bootstrap.mjs's doc comment describes for fetch's
# default timeout). Passing read_timeout: nil disables it: Net::HTTP hands
# nil straight to IO#wait_readable, which then blocks indefinitely instead
# of timing out.
def next_invocation(base)
  uri = URI("#{base}/invocation/next")

  response = Net::HTTP.start(uri.host, uri.port, read_timeout: nil) do |http|
    http.request(Net::HTTP::Get.new(uri))
  end

  request_id = response['Lambda-Runtime-Aws-Request-Id']
  deadline_ms = response['Lambda-Runtime-Deadline-Ms']
  body = response.body
  event = body && !body.empty? ? JSON.parse(body) : nil

  [request_id, deadline_ms ? deadline_ms.to_i : nil, event]
end

def main
  runtime_api = ENV['AWS_LAMBDA_RUNTIME_API']
  handler_spec = ARGV[0]

  if !runtime_api || runtime_api.empty? || !handler_spec
    warn 'bootstrap.rb: AWS_LAMBDA_RUNTIME_API env var and a handler argument are required'
    exit 1
  end

  base = "http://#{runtime_api}/2018-06-01/runtime"

  begin
    handler_fn = load_handler(handler_spec)
  rescue StandardError, ScriptError => e
    # ScriptError (LoadError's superclass) is deliberately caught alongside
    # StandardError: `require file` raises LoadError when the handler file
    # doesn't exist, and LoadError isn't a StandardError subclass in Ruby,
    # unlike Python's ImportError — a bad handler spec must still reach
    # init/error rather than crash the shim uncaught.
    post_json("#{base}/init/error", error_payload(e))
    exit 1
  end

  function_name = ENV['AWS_LAMBDA_FUNCTION_NAME'] || ''

  loop do
    request_id, deadline_ms, event = next_invocation(base)
    context = Context.new(request_id, function_name, deadline_ms)

    begin
      result = handler_fn.call(event: event, context: context)
      post_json("#{base}/invocation/#{request_id}/response", result)
    rescue StandardError => e
      post_json("#{base}/invocation/#{request_id}/error", error_payload(e))
    end
  end
end

main if $PROGRAM_NAME == __FILE__

package main

import (
	"example.com/gomodule/internal/greet"
	"example.com/gomodule/internal/runtime"
)

func main() {
	runtime.Serve(greet.Greeting("hello"))
}

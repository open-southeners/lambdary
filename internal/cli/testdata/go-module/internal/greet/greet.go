// Package greet is code shared by every function in the module, so an edit
// here must reach all of them.
package greet

// Version is stamped by lambdary.yml's build.go.ldflags.
var Version = "unset"

// Greeting is the text both functions answer with.
func Greeting(fn string) string {
	return fn + " says v1 (" + Version + ")"
}

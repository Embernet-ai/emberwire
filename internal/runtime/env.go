package runtime

import "os"

// lookupProcessEnv is the outermost scope of environment-variable resolution.
//
// It is a separate file so that a test can observe exactly which names a flow
// reaches out to the process environment for. Inside a container, that set is
// the app's real configuration surface.
func lookupProcessEnv(name string) (string, bool) {
	return os.LookupEnv(name)
}
